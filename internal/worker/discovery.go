package worker

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/gorizond/vcs-rag/internal/config"
	"github.com/gorizond/vcs-rag/internal/log"
	"github.com/gorizond/vcs-rag/internal/provider"
)

// snapshot is the last successful repository listing (for US2+ lanes).
type snapshot struct {
	mu    sync.Mutex
	repos []provider.Repo
}

func (s *snapshot) set(repos []provider.Repo) {
	s.mu.Lock()
	s.repos = repos
	s.mu.Unlock()
}

func (s *snapshot) get() []provider.Repo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.repos
}

// runDiscovery is the discovery lane: periodic repository listing
// (WORKER_INTERVAL). A listing error does not break the loop (FR-014).
func runDiscovery(ctx context.Context, p provider.VCSProvider, cfg *config.Config, logger *slog.Logger, snap *snapshot) error {
	logger = log.WithPhase(logger, "discovery")
	listOnce(ctx, p, logger, snap)

	ticker := time.NewTicker(cfg.Worker.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			listOnce(ctx, p, logger, snap)
		}
	}
}

// listOnce performs one pass: listing, a log line per repository,
// snapshot update. Errors (except context cancellation) are logged.
func listOnce(ctx context.Context, p provider.VCSProvider, logger *slog.Logger, snap *snapshot) {
	repos, err := p.ListRepositories(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return // normal shutdown — stay quiet
		}
		logger.Error("repository listing failed", "err", err)
		return
	}
	snap.set(repos)
	logger.Info("repositories fetched", "provider", p.Type(), "count", len(repos))
	for _, r := range repos {
		log.WithRepo(logger, r.Namespace+"/"+r.Path).Info("repo", "namespace", r.Namespace)
	}
}
