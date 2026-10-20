package worker

import (
	"context"
	"math/rand"
	"time"
)

// Adaptive lane pacing (T129): the configured env intervals are ceilings, not
// fixed ticks. A busy pass retries after the minimum delay; an idle lane backs
// off exponentially up to its ceiling; a VCS rate limit dominates with the
// provider's Retry-After.
const (
	paceMin       = 5 * time.Second
	paceRateFloor = 30 * time.Second // retryAfter fallback
	paceRateCap   = 5 * time.Minute
	paceJitter    = 0.25            // ±25 %
	paceStartMax  = 2 * time.Second // startup jitter for the index/meta lanes
)

// sleepCtx waits for d or until the context is done (shared by pacer/limiter).
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// pacer computes the delay after a lane pass; not safe for concurrent use.
type pacer struct {
	min, ceiling time.Duration
	// rateFloor/rateCap are the configured retry policy bounds (FR-007);
	// rateN counts consecutive rate-limit outcomes since the last non-rate-limit
	// result — the exponent of the growing retry interval.
	rateFloor, rateCap time.Duration
	rateN              int
	delay              time.Duration
	reason             string

	rnd   *rand.Rand
	sleep func(ctx context.Context, d time.Duration) error
}

// newPacer creates the pacer of a lane; ceiling = the lane's env interval.
func newPacer(ceiling time.Duration) *pacer {
	if ceiling < paceMin {
		ceiling = paceMin
	}
	return &pacer{
		min:       paceMin,
		ceiling:   ceiling,
		rateFloor: paceRateFloor,
		rateCap:   paceRateCap,
		delay:     paceMin,
		rnd:       rand.New(rand.NewSource(time.Now().UnixNano())),
		sleep:     sleepCtx,
	}
}

// newPacerWithLimits creates a pacer with the configured retry floor/cap
// (WORKER_UPSTREAM_RETRY_FLOOR/CAP); zero values keep the built-in defaults.
func newPacerWithLimits(ceiling, rateFloor, rateCap time.Duration) *pacer {
	p := newPacer(ceiling)
	if rateFloor > 0 {
		p.rateFloor = rateFloor
	}
	if rateCap > 0 {
		p.rateCap = rateCap
	}
	if p.rateCap < p.rateFloor {
		p.rateCap = p.rateFloor
	}
	return p
}

// startDelay returns the random startup delay U[0, 2s] (avoids a boot herd).
func (p *pacer) startDelay() time.Duration {
	return time.Duration(p.rnd.Float64() * float64(paceStartMax))
}

// next computes the delay after a pass: rate limit > busy > idle backoff.
// A rate limit starts from the base interval
// clamp(max(Retry-After, RetryFloor), RetryFloor, RetryCap); repeated
// consecutive rate-limit outcomes double it (FR-007) up to the cap.
// retryAfter <= 0 falls back to the floor; the result is capped and jittered.
func (p *pacer) next(busy, rateLimited bool, retryAfter time.Duration) time.Duration {
	if !rateLimited {
		p.rateN = 0 // a non-rate-limit result resets the growth (FR-007)
	}
	var d time.Duration
	switch {
	case rateLimited:
		d = p.rateWait(retryAfter) // already jittered and clamped (FR-007)
		p.rateN++
		p.delay = d
		p.reason = "rate-limit"
		return d
	case busy:
		d = p.min
		p.delay = d
		p.reason = "busy"
	default:
		d = p.delay * 2
		if d < p.min {
			d = p.min
		}
		if d > p.ceiling {
			d = p.ceiling
		}
		p.delay = d
		p.reason = "idle"
	}
	return p.withJitter(d)
}

// rateWait is the FR-007 retry interval:
//
//	wait₀ = clamp(max(Retry-After, RetryFloor), RetryFloor, RetryCap)
//	waitₙ = clamp(wait₀ · 2ⁿ + jitter, RetryFloor, RetryCap)
func (p *pacer) rateWait(retryAfter time.Duration) time.Duration {
	base := retryAfter
	if base < p.rateFloor {
		base = p.rateFloor
	}
	if base > p.rateCap {
		base = p.rateCap
	}
	d := base
	for i := 0; i < p.rateN; i++ {
		if d >= p.rateCap/2 {
			d = p.rateCap
			break
		}
		d *= 2
	}
	d = p.withJitter(d)
	if d < p.rateFloor {
		d = p.rateFloor
	}
	if d > p.rateCap {
		d = p.rateCap
	}
	return d
}

// withJitter spreads the delay by ±25 %.
func (p *pacer) withJitter(d time.Duration) time.Duration {
	f := 1 + paceJitter*(2*p.rnd.Float64()-1)
	return time.Duration(float64(d) * f)
}
