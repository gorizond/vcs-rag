package gitlab

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// T028/R3: the client parks until RateLimit-Reset only when Remaining < 10% of
// Limit; absent headers leave the 429-driven behavior unchanged.
func TestObserveRateLimitHeaders(t *testing.T) {
	tr := &upstreamTransport{base: http.DefaultTransport}
	tr.gate.Store(provider.NewUpstreamGate(1))
	reset := time.Now().Add(time.Hour).Truncate(time.Second)
	h := http.Header{}
	h.Set("RateLimit-Limit", "5000")
	h.Set("RateLimit-Remaining", "4999")
	h.Set("RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))

	tr.observe(h)
	if used, ok := tr.usage(); !ok || used >= 0.01 {
		t.Errorf("usage = %v/%v, want ~0 from the observed headers", used, ok)
	}
	tr.mu.Lock()
	until := tr.until
	tr.mu.Unlock()
	if !until.IsZero() {
		t.Fatalf("cooldown started with 99%% of the budget remaining: %v", until)
	}

	h.Set("RateLimit-Remaining", "100") // 2% < 10%
	tr.observe(h)
	tr.mu.Lock()
	until = tr.until
	tr.mu.Unlock()
	if !until.Equal(reset) {
		t.Errorf("cooldown until %v, want RateLimit-Reset %v", until, reset)
	}

	tr2 := &upstreamTransport{}
	tr2.observe(http.Header{})
	tr2.mu.Lock()
	until = tr2.until
	tr2.mu.Unlock()
	if !until.IsZero() {
		t.Error("headers absent must not start a cooldown")
	}
	if _, ok := tr2.usage(); ok {
		t.Error("headers absent must not report a usage")
	}
}

// T022: the narrow-endpoint bucket waits for a refill and honors ctx.
func TestTokenBucketWaitCancels(t *testing.T) {
	b := newTokenBucket(1) // one token per minute
	if err := b.wait(context.Background()); err != nil {
		t.Fatalf("first token: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := b.wait(ctx); err == nil {
		t.Error("an empty bucket must wait, not hand out a token")
	}
}
