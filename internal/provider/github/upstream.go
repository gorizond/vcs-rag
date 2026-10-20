// Upstream pacing of the GitHub client (FR-002/FR-003/FR-008): the shared
// concurrency gate, the bounded FR-011 queue, the proactive X-RateLimit-*
// cooldown, rate-limit event counting and per-page charging. This mirrors the
// GitLab transport with GitHub's header family and its 403 primary-rate-limit
// answer.
package github

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// rateSafetyPercent parks the client when the remaining budget drops below
// 10% of the limit (R3).
const rateSafetyPercent = 10

// upstreamTransport wraps every GitHub API request.
type upstreamTransport struct {
	base  http.RoundTripper
	gate  atomic.Pointer[provider.UpstreamGate]
	queue atomic.Pointer[provider.UpstreamQueue]
	pages atomic.Int64
	// FR-008: rate-limit events (primary 403 + secondary 429) and the last
	// Retry-After carried by a 429.
	rateEvents atomic.Int64
	retry      atomic.Int64 // nanoseconds

	chargerMu sync.Mutex
	charger   func()

	mu          sync.Mutex
	until       time.Time
	limit       int
	remaining   int
	haveHeaders bool
}

func (t *upstreamTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := t.waitCooldown(req.Context()); err != nil {
		return nil, err
	}
	release, err := t.acquire(req.Context())
	if err != nil {
		return nil, err
	}
	defer release()
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	t.pages.Add(1)
	t.observe(resp.Header)
	t.countRateLimit(resp)
	t.charge(req.Context())
	return resp, nil
}

// acquire takes one slot of the shared queue (gate + FR-011 bound) when the
// worker wired it; without a queue it falls back to the plain gate. An
// overflow returns provider.ErrTooManyTasks without waiting (T063).
func (t *upstreamTransport) acquire(ctx context.Context) (func(), error) {
	if q := t.queue.Load(); q != nil {
		return q.Acquire(ctx)
	}
	return t.gate.Load().Acquire(ctx)
}

// waitCooldown blocks until the proactive rate-limit pause expires (R3).
func (t *upstreamTransport) waitCooldown(ctx context.Context) error {
	t.mu.Lock()
	until := t.until
	t.mu.Unlock()
	d := time.Until(until)
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// observe implements R3 over GitHub's X-RateLimit-* family: when Remaining <
// 10% of Limit, park until Reset (unix seconds). Absent headers — unchanged.
func (t *upstreamTransport) observe(h http.Header) {
	if h.Get("X-RateLimit-Limit") == "" || h.Get("X-RateLimit-Remaining") == "" {
		return
	}
	limit := headerInt(h, "X-RateLimit-Limit")
	remaining := headerInt(h, "X-RateLimit-Remaining")
	if limit <= 0 || remaining < 0 {
		return
	}
	reset := headerInt(h, "X-RateLimit-Reset")
	t.mu.Lock()
	t.limit, t.remaining, t.haveHeaders = limit, remaining, true
	if remaining*100 < limit*rateSafetyPercent && reset > 0 {
		if until := time.Unix(int64(reset), 0); until.After(time.Now()) {
			t.until = until
		}
	}
	t.mu.Unlock()
}

// countRateLimit counts GitHub's two limit answers: the primary limit is a 403
// with an exhausted X-RateLimit-Remaining (or Retry-After), the secondary one
// is a 429 carrying Retry-After.
func (t *upstreamTransport) countRateLimit(resp *http.Response) {
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		t.rateEvents.Add(1)
		if d := retryAfterDuration(resp.Header); d > 0 {
			t.retry.Store(int64(d))
		}
	case resp.StatusCode == http.StatusForbidden &&
		(resp.Header.Get("X-RateLimit-Remaining") == "0" || resp.Header.Get("Retry-After") != ""):
		t.rateEvents.Add(1)
		if d := retryAfterDuration(resp.Header); d > 0 {
			t.retry.Store(int64(d))
		}
	}
}

func (t *upstreamTransport) usage() (float64, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.haveHeaders || t.limit <= 0 {
		return 0, false
	}
	return float64(t.limit-t.remaining) / float64(t.limit), true
}

// charge consumes exactly one page unit: the per-pass context charger wins
// over the client-wide fallback, so concurrent lanes never double-charge a
// page (T064).
func (t *upstreamTransport) charge(ctx context.Context) {
	if provider.ChargePage(ctx) {
		return
	}
	t.chargerMu.Lock()
	fn := t.charger
	t.chargerMu.Unlock()
	if fn != nil {
		fn()
	}
}

func (t *upstreamTransport) setPageCharger(fn func()) {
	t.chargerMu.Lock()
	t.charger = fn
	t.chargerMu.Unlock()
}

func headerInt(h http.Header, name string) int {
	n, err := strconv.Atoi(h.Get(name))
	if err != nil {
		return 0
	}
	return n
}

// retryAfterDuration parses Retry-After: seconds or an HTTP-date (0 — none).
func retryAfterDuration(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if at, err := http.ParseTime(v); err == nil && time.Until(at) > 0 {
		return time.Until(at)
	}
	return 0
}
