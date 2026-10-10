package worker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// maxRateLimitPause is the upper bound of the pause on a VCS rate limit (FR-015).
const maxRateLimitPause = 5 * time.Minute

// processSafely runs per-repo processing with failure isolation (FR-014):
// a panic does not break the lane, an error is logged with structure (repo,
// phase) and does NOT change state — retried on the next pass; rate limit
// errors wait min(retryAfter, maxRateLimitPause) before retry (FR-015).
// It returns the processing error (nil — success or normal context cancellation).
func processSafely(ctx context.Context, lg *slog.Logger, repo provider.Repo, phase string, fn func() error) (err error) {
	name := repo.Namespace + "/" + repo.Path
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
			lg.Error("panic while processing repository", "repo", name, "phase", phase, "panic", fmt.Sprint(r))
		}
	}()

	err = fn()
	if err == nil || ctx.Err() != nil {
		return nil // normal shutdown — stay quiet
	}
	if retryAfter, ok := provider.IsRateLimit(err); ok {
		pause := retryAfter
		if pause <= 0 || pause > maxRateLimitPause {
			pause = maxRateLimitPause
		}
		lg.Warn("VCS rate limit: pause before retry", "repo", name, "phase", phase,
			"retry_after", retryAfter.String(), "pause", pause.String())
		select {
		case <-ctx.Done():
		case <-time.After(pause):
		}
		return err
	}
	lg.Error("repository processing failed", "repo", name, "phase", phase, "err", err)
	return err
}
