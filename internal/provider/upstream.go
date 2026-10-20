package provider

import (
	"context"
	"errors"
	"sync/atomic"
	"time"
)

// UpstreamGate is the instance-wide bound on concurrent outbound operations
// (FR-002/FR-011): git transport and API traffic draw from one semaphore.
// A nil gate is a no-op (narrow unit tests build zero-value deps).
type UpstreamGate struct {
	sem chan struct{}
	n   int
}

// NewUpstreamGate creates the gate; n <= 0 falls back to the conservative
// default of 2 (FR-010: "forgot to configure" protection).
func NewUpstreamGate(n int) *UpstreamGate {
	if n <= 0 {
		n = 2
	}
	return &UpstreamGate{sem: make(chan struct{}, n), n: n}
}

// Cap returns the concurrency of the gate (0 for a nil gate).
func (g *UpstreamGate) Cap() int {
	if g == nil {
		return 0
	}
	return g.n
}

// Acquire takes one slot; the returned release must be called exactly once.
// The wait respects context cancellation.
func (g *UpstreamGate) Acquire(ctx context.Context) (func(), error) {
	if g == nil {
		return func() {}, nil
	}
	select {
	case g.sem <- struct{}{}:
		return func() { <-g.sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// ErrTooManyTasks is the honest FR-011 overflow error of the bounded upstream
// queue: queued work is never dropped, only new work is rejected.
var ErrTooManyTasks = errors.New("too many tasks")

// UpstreamQueue is the bounded waiting room in front of the shared gate: at
// most UpstreamConcurrency operations run and at most UpstreamQueueLimit wait;
// the (concurrency+limit)-th pending operation is rejected with
// ErrTooManyTasks. Waiters that already entered are never lost. Both API
// transports and the worker git lane use this one queue.
type UpstreamQueue struct {
	gate    *UpstreamGate
	limit   int
	pending atomic.Int64
}

// NewUpstreamQueue sizes the queue; limit <= 0 falls back to the FR-010
// default of 1000 (a zero-value unit-test config).
func NewUpstreamQueue(gate *UpstreamGate, limit int) *UpstreamQueue {
	if limit <= 0 {
		limit = 1000
	}
	return &UpstreamQueue{gate: gate, limit: gate.Cap() + limit}
}

// Acquire admits one operation (running or waiting) and returns its release.
// Overflow returns ErrTooManyTasks immediately — the caller must not wait.
func (q *UpstreamQueue) Acquire(ctx context.Context) (func(), error) {
	if q == nil {
		return func() {}, nil
	}
	if q.pending.Add(1) > int64(q.limit) {
		q.pending.Add(-1)
		return nil, ErrTooManyTasks
	}
	release, err := q.gate.Acquire(ctx)
	if err != nil {
		q.pending.Add(-1)
		return nil, err
	}
	return func() {
		release()
		q.pending.Add(-1)
	}, nil
}

// Run executes one operation under the queue.
func (q *UpstreamQueue) Run(ctx context.Context, fn func() error) error {
	release, err := q.Acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	return fn()
}

// Pending reports the running+waiting operations (tests/observability).
func (q *UpstreamQueue) Pending() int64 {
	if q == nil {
		return 0
	}
	return q.pending.Load()
}

// RateLimitObserver is an optional provider capability (FR-015): used is the
// fraction [0,1] of the authenticated account budget consumed, read from the
// RateLimit-* headers of the last response. ok=false — headers were absent.
type RateLimitObserver interface {
	RateUsage() (used float64, ok bool)
}

// PageCounter is an optional provider capability (FR-003): the number of API
// page responses served since the client started. The worker charges pages to
// the per-pass unit budget.
type PageCounter interface {
	Pages() int64
}

// PageCharger is an optional provider capability (FR-003): the worker installs
// a charger that consumes one pass unit per API page response; nil removes it.
type PageCharger interface {
	SetPageCharger(func())
}

// RateLimitEventCounter is an optional provider capability (FR-008): the number
// of rate-limit responses observed since the client started.
type RateLimitEventCounter interface {
	RateLimitEvents() int64
}

// RetryAfterReporter is an optional provider capability (FR-008): the last
// Retry-After duration carried by a rate-limit response (0 — none).
type RetryAfterReporter interface {
	LastRetryAfter() time.Duration
}

// pageChargerKey is the context key of a per-pass page charger.
type pageChargerKey struct{}

// WithPageCharger attaches a per-pass page charger to ctx. The API transport
// calls it exactly once per page response; this per-pass attachment lets
// concurrent lanes charge their own budgets without a shared single slot.
func WithPageCharger(ctx context.Context, fn func()) context.Context {
	return context.WithValue(ctx, pageChargerKey{}, fn)
}

// ChargePage consumes the context charger, reporting whether it existed. A
// transport calls this once per API page: the context charger takes precedence
// over the client-wide SetPageCharger fallback, so one page is never charged
// to two passes.
func ChargePage(ctx context.Context) bool {
	fn, ok := ctx.Value(pageChargerKey{}).(func())
	if !ok || fn == nil {
		return false
	}
	fn()
	return true
}
