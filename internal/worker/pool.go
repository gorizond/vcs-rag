package worker

import "context"

// pool — семафор ограничения параллелизма обработки репозиториев
// (WORKER_CONCURRENCY, T070).
type pool struct {
	sem chan struct{}
}

// newPool создаёт пул на n слотов (n<1 → 1).
func newPool(n int) *pool {
	if n < 1 {
		n = 1
	}
	return &pool{sem: make(chan struct{}, n)}
}

// Acquire берёт слот; отмена контекста прерывает ожидание.
func (p *pool) Acquire(ctx context.Context) error {
	select {
	case p.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Release освобождает слот.
func (p *pool) Release() { <-p.sem }
