package worker

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/gorizond/vcs-rag/internal/embed"
)

// AIMD parameters of the embeddings limiter (T128).
const (
	limiterRampStreak    = 10               // fast successes in a row → limit +1
	limiterFastLatency   = 10 * time.Second // a "fast" success
	limiterBaseCooldown  = 5 * time.Second  // throttle cooldown: 5s·2^(k-1)
	limiterMaxCooldown   = 60 * time.Second
	limiterLatencyWindow = 64
	limiterSlotPoll      = 10 * time.Millisecond // slot-wait polling step
)

// limiterStats is a snapshot for the pass summary (T131).
type limiterStats struct {
	Limit     int
	Reason    string
	P50       time.Duration
	P95       time.Duration
	Throttles int64
}

// limiter is a dynamic AIMD concurrency limiter around the embeddings provider:
// all worker-side Embed calls (background lanes and the refresh composition) go
// through it. It increases the limit additively after a streak of fast
// successes and halves it with an exponential cooldown on throttle signals
// (embed.IsThrottle, T127) — one local model is protected from overload.
type limiter struct {
	inner  embed.Provider
	logger *slog.Logger

	mu            sync.Mutex
	max           int // ceiling: WORKER_CONCURRENCY
	limit         int
	inflight      int
	streak        int
	throttleK     int
	cooldownUntil time.Time
	throttles     int64
	latencies     []time.Duration
	latPos        int
	reason        string

	// injectable for tests (default: the real clock/timers).
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

// newLimiter wraps the provider; max (the ceiling) is WORKER_CONCURRENCY.
func newLimiter(inner embed.Provider, max int, logger *slog.Logger) *limiter {
	l := &limiter{
		inner:  inner,
		logger: logger,
		max:    max,
		limit:  max,
		now:    time.Now,
		sleep:  sleepCtx,
	}
	l.mu.Lock()
	l.normalizeLocked()
	l.mu.Unlock()
	return l
}

// Dim passes the dimension through.
func (l *limiter) Dim() int { return l.inner.Dim() }

// Embed runs one embeddings call under the dynamic limit and the cooldown.
func (l *limiter) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if err := l.acquire(ctx); err != nil {
		return nil, err
	}
	start := l.now()
	vecs, err := l.inner.Embed(ctx, texts)
	l.release(err, l.now().Sub(start))
	return vecs, err
}

// acquire waits for a free slot and for the end of a throttle cooldown.
// ponytail: slot waits poll with a small step — the contention is a handful of
// worker goroutines, and a cond-var cannot express the ctx-aware cooldown wait.
func (l *limiter) acquire(ctx context.Context) error {
	for {
		l.mu.Lock()
		if now := l.now(); now.Before(l.cooldownUntil) {
			wait := l.cooldownUntil.Sub(now)
			l.mu.Unlock()
			if err := l.sleep(ctx, wait); err != nil {
				return err
			}
			continue
		}
		if l.inflight < l.limit {
			l.inflight++
			l.mu.Unlock()
			return nil
		}
		l.mu.Unlock()
		if err := l.sleep(ctx, limiterSlotPoll); err != nil {
			return err
		}
	}
}

// release records the outcome and adjusts the limit (AIMD).
func (l *limiter) release(err error, lat time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.inflight--

	if embed.IsThrottle(err) {
		old := l.limit
		l.limit /= 2
		l.normalizeLocked()
		l.streak = 0
		l.throttles++
		l.throttleK++
		cd := limiterMaxCooldown
		if l.throttleK <= 4 {
			cd = limiterBaseCooldown << (l.throttleK - 1) // 5s, 10s, 20s, 40s
		}
		l.cooldownUntil = l.now().Add(cd)
		l.reason = "throttle"
		if l.limit != old {
			l.logger.Info("embed capacity changed", "from", old, "to", l.limit,
				"reason", "throttle", "cooldown", cd.String())
		}
		return
	}
	if err != nil {
		l.streak = 0
		l.reason = "error"
		return
	}
	l.recordLatency(lat)
	l.throttleK = 0
	if lat >= limiterFastLatency {
		l.streak = 0 // only a streak of FAST successes ramps the limit up
		l.reason = "slow"
		return
	}
	l.streak++
	if l.streak >= limiterRampStreak && l.limit < l.max {
		old := l.limit
		l.limit++
		l.streak = 0
		l.reason = "ramp"
		l.logger.Info("embed capacity changed", "from", old, "to", l.limit, "reason", "ramp")
		return
	}
	if l.reason == "" {
		l.reason = "ok"
	}
}

// normalizeLocked clamps the limit into [1, max].
func (l *limiter) normalizeLocked() {
	if l.max < 1 {
		l.max = 1
	}
	if l.limit > l.max {
		l.limit = l.max
	}
	if l.limit < 1 {
		l.limit = 1
	}
}

// recordLatency stores the latency in the ring buffer (T131 percentiles).
func (l *limiter) recordLatency(d time.Duration) {
	if l.latencies == nil {
		l.latencies = make([]time.Duration, limiterLatencyWindow)
	}
	l.latencies[l.latPos%limiterLatencyWindow] = d
	l.latPos++
}

// stats snapshots the limiter for the pass summary (T131).
func (l *limiter) stats() limiterStats {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := limiterStats{Limit: l.limit, Reason: l.reason, Throttles: l.throttles}
	n := l.latPos
	if n > len(l.latencies) {
		n = len(l.latencies)
	}
	if n == 0 {
		return st
	}
	sample := append([]time.Duration(nil), l.latencies[:n]...)
	sort.Slice(sample, func(i, j int) bool { return sample[i] < sample[j] })
	st.P50 = sample[(len(sample)*50)/100]
	st.P95 = sample[min(len(sample)-1, (len(sample)*95)/100)]
	return st
}
