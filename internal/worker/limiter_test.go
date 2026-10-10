package worker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gorizond/vcs-rag/internal/embed"
)

// stubEmbed is a controllable embed.Provider for the limiter tests.
type stubEmbed struct {
	mu     sync.Mutex
	err    error
	onCall func()
	calls  int
}

func (s *stubEmbed) Dim() int { return 2 }

func (s *stubEmbed) Embed(_ context.Context, texts []string) ([][]float32, error) {
	s.mu.Lock()
	s.calls++
	hook, err := s.onCall, s.err
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	if err != nil {
		return nil, err
	}
	vecs := make([][]float32, len(texts))
	for i := range vecs {
		vecs[i] = []float32{1, 2}
	}
	return vecs, nil
}

func (s *stubEmbed) setErr(err error) {
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
}

// fakeClock is a deterministic time source for the cooldown/ramp tests.
type fakeClock struct {
	mu    sync.Mutex
	t     time.Time
	slept []time.Duration
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Unix(1000, 0)} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// sleep advances the clock instantly and records the duration.
func (c *fakeClock) sleep(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
	c.slept = append(c.slept, d)
	return nil
}

func (c *fakeClock) sleeps() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.slept...)
}

func (c *fakeClock) resetSleeps() {
	c.mu.Lock()
	c.slept = nil
	c.mu.Unlock()
}

func newLimiterForTest(t *testing.T, inner embed.Provider, max int) (*limiter, *fakeClock) {
	t.Helper()
	c := newFakeClock()
	l := newLimiter(inner, max, testLogger())
	l.now = c.now
	l.sleep = c.sleep
	return l, c
}

func throttleErr() error {
	return &embed.ThrottleError{Provider: "test", Reason: "timeout"}
}

// T128: halve on throttle (floor 1), ramp +1 per 10 fast successes, capped.
func TestLimiterHalveRampFloor(t *testing.T) {
	ctx := context.Background()
	inner := &stubEmbed{}
	l, _ := newLimiterForTest(t, inner, 4) // starts at the ceiling
	if got := l.stats().Limit; got != 4 {
		t.Fatalf("initial limit = %d, want 4", got)
	}

	inner.setErr(throttleErr())
	_, _ = l.Embed(ctx, []string{"a"}) // 4 → 2
	if got, st := l.stats().Limit, l.stats(); got != 2 || st.Throttles != 1 {
		t.Fatalf("after throttle: limit = %d throttles = %d, want 2/1", got, st.Throttles)
	}
	inner.setErr(nil)

	for i := 1; i <= limiterRampStreak; i++ {
		_, _ = l.Embed(ctx, []string{"a"})
		want := 2
		if i == limiterRampStreak {
			want = 3
		}
		if got := l.stats().Limit; got != want {
			t.Fatalf("after %d fast successes: limit = %d, want %d", i, got, want)
		}
	}
	if got := l.stats().Reason; got != "ramp" {
		t.Errorf("reason = %q, want ramp", got)
	}

	// the next streak ramps to the ceiling and stops there
	for i := 0; i < limiterRampStreak; i++ {
		_, _ = l.Embed(ctx, []string{"a"})
	}
	if got := l.stats().Limit; got != 4 {
		t.Fatalf("ramp to the ceiling: limit = %d, want 4", got)
	}
	for i := 0; i < limiterRampStreak; i++ {
		_, _ = l.Embed(ctx, []string{"a"})
	}
	if got := l.stats().Limit; got != 4 {
		t.Fatalf("limit above the ceiling: %d", got)
	}
}

// T128: throttles halve with the floor 1; a non-throttle error keeps the limit.
func TestLimiterNonThrottleKeepsLimit(t *testing.T) {
	ctx := context.Background()
	inner := &stubEmbed{}
	l, c := newLimiterForTest(t, inner, 4)

	inner.setErr(errors.New("HTTP 401: unauthorized"))
	_, _ = l.Embed(ctx, []string{"a"})
	st := l.stats()
	if st.Limit != 4 || st.Throttles != 0 || st.Reason != "error" {
		t.Errorf("after a permanent error: %+v, want limit 4 / no throttle", st)
	}
	if len(c.sleeps()) != 0 {
		t.Errorf("cooldown on a permanent error: slept %v", c.sleeps())
	}

	// two consecutive throttles: 4 → 2 → 1 (floor), cooldowns 5s then 10s.
	inner.setErr(throttleErr())
	_, _ = l.Embed(ctx, []string{"a"})
	_, _ = l.Embed(ctx, []string{"a"})
	if got := l.stats().Limit; got != 1 {
		t.Fatalf("limit = %d, want the floor 1", got)
	}
	if got := c.sleeps(); len(got) != 1 || got[0] != limiterBaseCooldown {
		t.Fatalf("cooldown sleeps = %v, want [%v]", got, limiterBaseCooldown)
	}
}

// T128: the cooldown grows 5s·2^(k−1) and caps at 60s; a success resets the
// growth. Each throttle call first waits out the cooldown (recorded by the
// fake clock).
func TestLimiterCooldownGrowthAndCap(t *testing.T) {
	ctx := context.Background()
	inner := &stubEmbed{}
	inner.setErr(throttleErr())
	l, c := newLimiterForTest(t, inner, 8)

	for i := 0; i < 6; i++ {
		_, _ = l.Embed(ctx, []string{"a"})
	}
	got := c.sleeps()
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 60 * time.Second}
	if len(got) != len(want) {
		t.Fatalf("cooldown waits = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("cooldown #%d = %v, want %v", i+1, got[i], want[i])
		}
	}
	if st := l.stats(); st.Limit != 1 {
		t.Errorf("limit after 6 throttles = %d, want 1 (floor)", st.Limit)
	}

	// a fast success resets the cooldown growth (k → 0): the success call itself
	// still waits out the previous 60s cap, then the next throttle cycle starts
	// again from the base 5s.
	c.resetSleeps()
	inner.setErr(nil)
	_, _ = l.Embed(ctx, []string{"a"}) // waits the tail of the cap
	inner.setErr(throttleErr())
	_, _ = l.Embed(ctx, []string{"a"}) // no wait: the cooldown already elapsed
	_, _ = l.Embed(ctx, []string{"a"}) // waits the reset base cooldown
	got = c.sleeps()
	if len(got) != 2 || got[0] != limiterMaxCooldown || got[1] != limiterBaseCooldown {
		t.Errorf("after a success: sleeps = %v, want [%v %v] (growth reset to the base)",
			got, limiterMaxCooldown, limiterBaseCooldown)
	}
}

// T128: a slow success breaks the fast streak.
func TestLimiterSlowSuccessNoRamp(t *testing.T) {
	ctx := context.Background()
	inner := &stubEmbed{}
	l, c := newLimiterForTest(t, inner, 4)
	inner.setErr(throttleErr())
	_, _ = l.Embed(ctx, []string{"a"}) // 4 → 2
	inner.setErr(nil)                  // a success resets throttleK

	inner.onCall = func() { c.mu.Lock(); c.t = c.t.Add(11 * time.Second); c.mu.Unlock() }
	for i := 0; i < 3*limiterRampStreak; i++ {
		_, _ = l.Embed(ctx, []string{"a"})
	}
	if got, st := l.stats().Limit, l.stats(); got != 2 || st.Reason != "slow" {
		t.Errorf("limit = %d reason = %q, want 2/slow (slow successes do not ramp)", got, st.Reason)
	}
}

// T128: normalization clamps the limit into [1, max].
func TestLimiterNormalize(t *testing.T) {
	l := newLimiter(&stubEmbed{}, 0, testLogger())
	if got := l.stats().Limit; got != 1 {
		t.Errorf("max=0 → limit = %d, want 1", got)
	}

	l2 := newLimiter(&stubEmbed{}, 2, testLogger())
	l2.mu.Lock()
	l2.limit = 9
	l2.normalizeLocked()
	got := l2.limit
	l2.limit = 0
	l2.normalizeLocked()
	low := l2.limit
	l2.mu.Unlock()
	if got != 2 || low != 1 {
		t.Errorf("normalize: 9→%d (want 2), 0→%d (want 1)", got, low)
	}
}

// T128/T131: percentile snapshot of the recent latencies.
func TestLimiterStatsPercentiles(t *testing.T) {
	ctx := context.Background()
	inner := &stubEmbed{}
	l, c := newLimiterForTest(t, inner, 4)
	inner.onCall = func() { c.mu.Lock(); c.t = c.t.Add(time.Second); c.mu.Unlock() } // 1s per call
	for i := 0; i < 20; i++ {
		_, _ = l.Embed(ctx, []string{"a"})
	}
	st := l.stats()
	if st.P50 != time.Second || st.P95 != time.Second {
		t.Errorf("p50/p95 = %v/%v, want 1s/1s", st.P50, st.P95)
	}
}
