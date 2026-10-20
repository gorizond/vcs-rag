package worker

import (
	"context"

	"github.com/gorizond/vcs-rag/internal/config"
	"github.com/gorizond/vcs-rag/internal/provider"
)

// ErrTooManyTasks is the honest FR-011 overflow error of the bounded upstream
// queue: queued work is never dropped, only new work is rejected.
var ErrTooManyTasks = provider.ErrTooManyTasks

// upstreamQueue is the worker-side handle of the shared provider queue: the
// same queue bounds git operations and API requests (T063).
type upstreamQueue struct {
	*provider.UpstreamQueue
}

// newUpstreamQueue sizes the queue; limit <= 0 falls back to the FR-010
// default of 1000 (a zero-value unit-test config).
func newUpstreamQueue(gate *provider.UpstreamGate, limit int) *upstreamQueue {
	return &upstreamQueue{provider.NewUpstreamQueue(gate, limit)}
}

// upstreamFor builds the one gate and queue shared by all lanes (FR-002).
func upstreamFor(cfg *config.Config) (*provider.UpstreamGate, *upstreamQueue) {
	w := passConfig(cfg)
	gate := provider.NewUpstreamGate(w.UpstreamConcurrency)
	return gate, newUpstreamQueue(gate, w.UpstreamQueueLimit)
}

// run executes one upstream operation under the gate; overflow is rejected
// before waiting (FR-011). A nil queue does not throttle (narrow unit tests).
func (q *upstreamQueue) run(ctx context.Context, fn func() error) error {
	if q == nil {
		return fn()
	}
	return q.Run(ctx, fn)
}

// runUpstream routes one outbound (git clone/fetch) operation through the
// instance-wide queue; without wiring it is a no-op.
func (d RepoDeps) runUpstream(ctx context.Context, fn func() error) error {
	if d.Queue != nil {
		return d.Queue.run(ctx, fn)
	}
	if d.Upstream != nil {
		release, err := d.Upstream.Acquire(ctx)
		if err != nil {
			return err
		}
		defer release()
	}
	return fn()
}
