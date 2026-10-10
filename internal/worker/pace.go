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
	delay        time.Duration
	reason       string

	rnd   *rand.Rand
	sleep func(ctx context.Context, d time.Duration) error
}

// newPacer creates the pacer of a lane; ceiling = the lane's env interval.
func newPacer(ceiling time.Duration) *pacer {
	if ceiling < paceMin {
		ceiling = paceMin
	}
	return &pacer{
		min:     paceMin,
		ceiling: ceiling,
		delay:   paceMin,
		rnd:     rand.New(rand.NewSource(time.Now().UnixNano())),
		sleep:   sleepCtx,
	}
}

// startDelay returns the random startup delay U[0, 2s] (avoids a boot herd).
func (p *pacer) startDelay() time.Duration {
	return time.Duration(p.rnd.Float64() * float64(paceStartMax))
}

// next computes the delay after a pass: rate limit > busy > idle backoff.
// retryAfter <= 0 falls back to 30s; the result is capped at 5m and jittered.
func (p *pacer) next(busy, rateLimited bool, retryAfter time.Duration) time.Duration {
	var d time.Duration
	switch {
	case rateLimited:
		d = retryAfter
		if d <= 0 {
			d = paceRateFloor
		}
		if d < p.min {
			d = p.min
		}
		if d > paceRateCap {
			d = paceRateCap
		}
		p.delay = d
		p.reason = "rate-limit"
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

// withJitter spreads the delay by ±25 %.
func (p *pacer) withJitter(d time.Duration) time.Duration {
	f := 1 + paceJitter*(2*p.rnd.Float64()-1)
	return time.Duration(float64(d) * f)
}
