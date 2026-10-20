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

// runMirror is the continuous mirror refresh lane (T040): an adaptive paced
// loop (T129) — WORKER_MIRROR_INTERVAL (default WORKER_INTERVAL) is the
// ceiling, not a tick; concurrency — WORKER_CONCURRENCY. Errors do not break
// the loop (FR-014).
func runMirror(ctx context.Context, cfg *config.Config, vcsType string, eng *gitengine.Engine, logger *slog.Logger, snap *snapshot) error {
	logger = log.WithPhase(logger, "mirror")
	p := newPacerWithLimits(cfg.Worker.MirrorInterval, cfg.Worker.RetryFloor, cfg.Worker.RetryCap)
	lastRepos := -1
	for {
		sum := mirrorAll(ctx, cfg, vcsType, eng, logger, snap)
		busy := sum != nil && sum.failed.Load() > 0
		rl, retry := false, time.Duration(0)
		if sum != nil {
			rl, retry = sum.rateLimited()
		}
		if n := len(snap.get()); n > 0 && lastRepos >= 0 && n != lastRepos {
			busy = true // new repositories appeared — mirror them immediately
		}
		lastRepos = len(snap.get())
		delay := p.next(busy, rl, retry)
		if sum != nil {
			sum.withPace(delay, p.reason)
			sum.log(logger)
		}
		if err := p.sleep(ctx, delay); err != nil {
			return nil
		}
	}
}

// mirrorAll refreshes mirrors of all known repositories through the pool;
// failures are isolated (FR-014). It returns the pass summary (nil — nothing to
// do) for the pacer and the pass log (T129).
func mirrorAll(ctx context.Context, cfg *config.Config, vcsType string, eng *gitengine.Engine, logger *slog.Logger, snap *snapshot) *summary {
	repos := snap.get()
	if len(repos) == 0 {
		return nil
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
	return sum
}

// mirrorRepo mirrors a single repository and records LastFetchAt; the per-repo
// lock (T110) serializes it with the index/metadata lanes and the refresh.
func mirrorRepo(ctx context.Context, vcsType string, eng *gitengine.Engine, logger *slog.Logger, repo provider.Repo) error {
	return withRepoLock(repoSlug(repo.Namespace+"/"+repo.Path), func() error {
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
	})
}
