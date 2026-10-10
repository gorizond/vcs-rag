package worker

import (
	"errors"
	"math/rand"
	"testing"
	"time"

	gh "github.com/google/go-github/v92/github"
)

// seededPacer returns a deterministic pacer (fixed jitter source).
func seededPacer(ceiling time.Duration) *pacer {
	p := newPacer(ceiling)
	p.rnd = rand.New(rand.NewSource(7))
	return p
}

// within reports whether d is inside ±25 % of target.
func within(d, target time.Duration) bool {
	lo := time.Duration(float64(target) * 0.75)
	hi := time.Duration(float64(target) * 1.25)
	return d >= lo && d <= hi
}

// T129: a busy pass retries after the 5s minimum (± jitter).
func TestPacerBusy(t *testing.T) {
	p := seededPacer(time.Minute)
	for i := 0; i < 5; i++ {
		d := p.next(true, false, 0)
		if !within(d, paceMin) {
			t.Fatalf("busy delay = %v, want ~%v", d, paceMin)
		}
		if p.reason != "busy" {
			t.Fatalf("reason = %q, want busy", p.reason)
		}
	}
}

// T129: an idle lane backs off ×2 up to the ceiling.
func TestPacerIdleBackoff(t *testing.T) {
	p := seededPacer(time.Minute)
	want := []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, time.Minute, time.Minute}
	for i, target := range want {
		d := p.next(false, false, 0)
		if !within(d, target) {
			t.Fatalf("idle #%d delay = %v, want ~%v", i+1, d, target)
		}
		if p.reason != "idle" {
			t.Fatalf("reason = %q, want idle", p.reason)
		}
	}

	// a busy pass snaps the delay back to the minimum.
	if d := p.next(true, false, 0); !within(d, paceMin) {
		t.Fatalf("busy after idle = %v, want ~%v", d, paceMin)
	}
	// and the idle chain doubles again from the minimum.
	if d := p.next(false, false, 0); !within(d, 10*time.Second) {
		t.Fatalf("idle after busy = %v, want ~10s", d)
	}
}

// T129: a VCS rate limit dominates: max(min, retryAfter||30s), capped at 5m.
func TestPacerRateLimit(t *testing.T) {
	p := seededPacer(time.Minute)
	cases := []struct {
		retryAfter time.Duration
		want       time.Duration
	}{
		{45 * time.Second, 45 * time.Second},
		{0, paceRateFloor},
		{2 * time.Second, paceMin}, // below the floor → min
		{10 * time.Minute, paceRateCap},
	}
	for _, tc := range cases {
		d := p.next(false, true, tc.retryAfter)
		if !within(d, tc.want) {
			t.Errorf("rate limit retryAfter=%v: delay = %v, want ~%v", tc.retryAfter, d, tc.want)
		}
		if p.reason != "rate-limit" {
			t.Errorf("reason = %q, want rate-limit", p.reason)
		}
	}
}

// T129: a ceiling below the minimum is normalized to the minimum.
func TestPacerCeilingFloor(t *testing.T) {
	p := seededPacer(2 * time.Second)
	if p.ceiling != paceMin {
		t.Fatalf("ceiling = %v, want the %v floor", p.ceiling, paceMin)
	}
	for i := 0; i < 4; i++ {
		if d := p.next(false, false, 0); !within(d, paceMin) {
			t.Fatalf("idle delay = %v, want ~%v (the ceiling floor)", d, paceMin)
		}
	}
}

// T129: the startup delay is U[0, 2s].
func TestPacerStartDelay(t *testing.T) {
	p := seededPacer(time.Minute)
	max := time.Duration(0)
	for i := 0; i < 200; i++ {
		d := p.startDelay()
		if d < 0 || d > paceStartMax {
			t.Fatalf("start delay = %v, out of [0, %v]", d, paceStartMax)
		}
		if d > max {
			max = d
		}
	}
	if max <= 0 {
		t.Error("the startup delay never moved off zero")
	}
}

// T129: the summary carries the rate-limit signal of a pass (typed retry-after).
func TestSummaryRateLimitSignal(t *testing.T) {
	sum := newSummary("index")
	sum.countRepo(nil)
	if rl, _ := sum.rateLimited(); rl {
		t.Fatal("no rate limit expected")
	}
	retry := 7 * time.Second
	sum.countRepo(&gh.AbuseRateLimitError{RetryAfter: &retry})
	rl, got := sum.rateLimited()
	if !rl || got != retry {
		t.Fatalf("rateLimited = %v/%v, want true/%v", rl, got, retry)
	}
	if sum.failed.Load() != 1 || sum.ok.Load() != 1 {
		t.Fatalf("counters = ok %d failed %d", sum.ok.Load(), sum.failed.Load())
	}

	// a text-only 429 signal still flips the flag (0 duration → the 30s fallback
	// is the pacer's concern).
	sum2 := newSummary("index")
	sum2.countRepo(errors.New("git fetch: The requested URL returned error: 429"))
	if rl, d := sum2.rateLimited(); !rl || d != 0 {
		t.Fatalf("text 429: rateLimited = %v/%v, want true/0", rl, d)
	}
}
