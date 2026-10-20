// Upstream pacing of the GitLab client (FR-002/FR-003): narrow-endpoint token
// buckets, the instance-wide concurrency gate and the proactive RateLimit-*
// cooldown (R3). All stdlib: a token bucket and an HTTP transport wrapper.
package gitlab

import (
	"context"
	"net/http"
	"strconv"
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
	pages atomic.Int64

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
	release, err := t.gate.Load().Acquire(req.Context())
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
	t.charge()
	return resp, nil
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

func (t *upstreamTransport) charge() {
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
