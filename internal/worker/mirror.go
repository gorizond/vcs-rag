package worker

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/gorizond/vcs-rag/internal/config"
	"github.com/gorizond/vcs-rag/internal/gitengine"
	"github.com/gorizond/vcs-rag/internal/log"
	"github.com/gorizond/vcs-rag/internal/provider"
)

// runMirror is the continuous mirror refresh lane (T040): on the
// WORKER_MIRROR_INTERVAL tick (default WORKER_INTERVAL) it mirrors
// repositories from the discovery registry, concurrency — WORKER_CONCURRENCY;
// errors do not break the loop (FR-014).
func runMirror(ctx context.Context, cfg *config.Config, vcsType string, eng *gitengine.Engine, logger *slog.Logger, snap *snapshot) error {
	logger = log.WithPhase(logger, "mirror")
	mirrorAll(ctx, cfg, vcsType, eng, logger, snap)

	ticker := time.NewTicker(cfg.Worker.MirrorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			mirrorAll(ctx, cfg, vcsType, eng, logger, snap)
		}
	}
}

// mirrorAll refreshes mirrors of all known repositories through the pool;
// failures are isolated (FR-014), a summary is written after the pass (FR-020).
func mirrorAll(ctx context.Context, cfg *config.Config, vcsType string, eng *gitengine.Engine, logger *slog.Logger, snap *snapshot) {
	repos := snap.get()
	if len(repos) == 0 {
		return
	}
	sum := newSummary("mirror")
	p := newPool(cfg.Worker.Concurrency)
	var wg sync.WaitGroup
	for _, repo := range repos {
		wg.Add(1)
		go func(repo provider.Repo) {
			defer wg.Done()
			if err := p.Acquire(ctx); err != nil {
				return
			}
			defer p.Release()
			sum.countRepo(processSafely(ctx, logger, repo, "mirror", func() error {
				return mirrorRepo(ctx, vcsType, eng, logger, repo)
			}))
		}(repo)
	}
	wg.Wait()
	sum.log(logger)
}

// mirrorRepo mirrors a single repository and records LastFetchAt.
func mirrorRepo(ctx context.Context, vcsType string, eng *gitengine.Engine, logger *slog.Logger, repo provider.Repo) error {
	name := repo.Namespace + "/" + repo.Path
	mirror, err := eng.EnsureMirror(ctx, vcsType, repo)
	if err != nil {
		return fmt.Errorf("mirror: %w", err)
	}
	if err := updateState(gitengine.StatePath(mirror), func(st *MirrorState) {
		st.LastFetchAt = time.Now().UTC()
	}); err != nil {
		return fmt.Errorf("state: %w", err)
	}
	logger.Info("mirror updated", "repo", name)
	return nil
}
