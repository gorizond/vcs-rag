// Upstream pacing of the GitLab client (FR-002/FR-003): narrow-endpoint token
// buckets, the instance-wide concurrency gate and the proactive RateLimit-*
// cooldown (R3). All stdlib: a token bucket and an HTTP transport wrapper.
package gitlab

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

// Narrow-endpoint caps (R2): GET /groups is the hard 50/min limit; project
// listing is 600/min. Constants by design — no env override (ponytail, R2).
const (
	groupsPerMinute   = 50
	projectsPerMinute = 600
	rateSafetyPercent = 10 // park when Remaining < 10% of Limit (R3)
)

// tokenBucket is the minimal per-endpoint rate limiter: burst == per-minute
// cap, continuous refill. wait blocks until one token is available or ctx dies.
type tokenBucket struct {
	mu     sync.Mutex
	tokens float64
	burst  float64
	rate   float64 // tokens per second
	last   time.Time
}

func newTokenBucket(perMinute int) *tokenBucket {
	n := float64(perMinute)
	return &tokenBucket{tokens: n, burst: n, rate: n / 60, last: time.Now()}
}

func (b *tokenBucket) wait(ctx context.Context) error {
	for {
		b.mu.Lock()
		now := time.Now()
		b.tokens = min(b.burst, b.tokens+now.Sub(b.last).Seconds()*b.rate)
		b.last = now
		if b.tokens >= 1 {
			b.tokens--
			b.mu.Unlock()
			return nil
		}
		d := time.Duration((1 - b.tokens) / b.rate * float64(time.Second))
		b.mu.Unlock()
		timer := time.NewTimer(d)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// upstreamTransport wraps every GitLab API request: it honors the shared
// concurrency gate, parks the client until RateLimit-Reset when the remaining
// budget drops under 10% (R3), and counts pages for the pass unit budget.
type upstreamTransport struct {
	base  http.RoundTripper
	gate  atomic.Pointer[provider.UpstreamGate]
	queue atomic.Pointer[provider.UpstreamQueue]
	pages atomic.Int64
	// US4/FR-008: 429 responses and the last Retry-After seen on one.
	four29s atomic.Int64
	retry   atomic.Int64 // nanoseconds

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
	if resp.StatusCode == http.StatusTooManyRequests {
		t.four29s.Add(1)
		if d := retryAfterDuration(resp.Header); d > 0 {
			t.retry.Store(int64(d))
		}
	}
	t.observe(resp.Header)
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

// observe implements R3: when Remaining < 10% of Limit, park until Reset.
// Absent headers leave the behavior unchanged (429-driven only).
func (t *upstreamTransport) observe(h http.Header) {
	limit := headerInt(h, "RateLimit-Limit")
	remaining := headerInt(h, "RateLimit-Remaining")
	if limit <= 0 || remaining < 0 {
		return
	}
	reset := headerInt(h, "RateLimit-Reset")
	t.mu.Lock()
	t.limit, t.remaining, t.haveHeaders = limit, remaining, true
	if remaining*100 < limit*rateSafetyPercent && reset > 0 {
		if until := time.Unix(int64(reset), 0); until.After(time.Now()) {
			t.until = until
		}
	}
	t.mu.Unlock()
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
