package worker

import "context"

// pool is a semaphore limiting repository processing concurrency
// (WORKER_CONCURRENCY, T070).
type pool struct {
	sem chan struct{}
}

// newPool creates a pool with n slots (n<1 → 1).
func newPool(n int) *pool {
	if n < 1 {
		n = 1
	}
	return &pool{sem: make(chan struct{}, n)}
}

// Acquire takes a slot; context cancellation aborts the wait.
func (p *pool) Acquire(ctx context.Context) error {
	select {
	case p.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Release frees a slot.
func (p *pool) Release() { <-p.sem }
