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

// snapshot — последний удачный листинг репозиториев (для линий US2+).
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

// runDiscovery — линия discovery: периодический листинг репозиториев
// (интервал WORKER_INTERVAL). Ошибка листинга не прерывает цикл (FR-014).
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

// listOnce выполняет один проход: листинг, лог по каждому репозиторию,
// обновление снапшота. Ошибка (кроме отмены контекста) логируется.
func listOnce(ctx context.Context, p provider.VCSProvider, logger *slog.Logger, snap *snapshot) {
	repos, err := p.ListRepositories(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return // штатное завершение — не шумим
		}
		logger.Error("листинг репозиториев не удался", "err", err)
		return
	}
	snap.set(repos)
	logger.Info("репозитории получены", "provider", p.Type(), "count", len(repos))
	for _, r := range repos {
		log.WithRepo(logger, r.Namespace+"/"+r.Path).Info("repo", "namespace", r.Namespace)
	}
}
