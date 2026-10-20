package provider

import (
	"context"
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
// of 429 responses observed since the client started.
type RateLimitEventCounter interface {
	RateLimitEvents() int64
}

// RetryAfterReporter is an optional provider capability (FR-008): the last
// Retry-After duration carried by a 429 response (0 — none).
type RetryAfterReporter interface {
	LastRetryAfter() time.Duration
}
