package worker

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/gorizond/vcs-rag/internal/config"
	"github.com/gorizond/vcs-rag/internal/gitengine"
	"github.com/gorizond/vcs-rag/internal/log"
	"github.com/gorizond/vcs-rag/internal/provider"
)

// runMirror — линия постоянного обновления зеркал (T040): по тикеру
// WORKER_INTERVAL зеркалирует репозитории из реестра discovery, параллелизм —
// WORKER_CONCURRENCY; ошибки не прерывают цикл (FR-014).
func runMirror(ctx context.Context, cfg *config.Config, vcsType string, eng *gitengine.Engine, logger *slog.Logger, snap *snapshot) error {
	logger = log.WithPhase(logger, "mirror")
	mirrorAll(ctx, cfg, vcsType, eng, logger, snap)

	ticker := time.NewTicker(cfg.Worker.Interval)
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

// mirrorAll обновляет зеркала всех известных репозиториев через пул.
func mirrorAll(ctx context.Context, cfg *config.Config, vcsType string, eng *gitengine.Engine, logger *slog.Logger, snap *snapshot) {
	repos := snap.get()
	if len(repos) == 0 {
		return
	}
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
			mirrorRepo(ctx, vcsType, eng, logger, repo)
		}(repo)
	}
	wg.Wait()
}

// mirrorRepo зеркалирует один репозиторий и фиксирует LastFetchAt.
func mirrorRepo(ctx context.Context, vcsType string, eng *gitengine.Engine, logger *slog.Logger, repo provider.Repo) {
	name := repo.Namespace + "/" + repo.Path
	mirror, err := eng.EnsureMirror(ctx, vcsType, repo)
	if err != nil {
		logger.Error("зеркало: обновление не удалось", "repo", name, "err", err)
		return
	}
	if err := updateState(gitengine.StatePath(mirror), func(st *MirrorState) {
		st.LastFetchAt = time.Now().UTC()
	}); err != nil {
		logger.Error("state: запись не удалась", "repo", name, "err", err)
		return
	}
	logger.Info("зеркало обновлено", "repo", name)
}
