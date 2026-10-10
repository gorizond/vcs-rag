// Package worker — фоновые линии: discovery, mirror, index (FR-005/006/018).
package worker

import (
	"context"
	"log/slog"
	"sync"

	"github.com/gorizond/vcs-rag/internal/config"
	"github.com/gorizond/vcs-rag/internal/embed"
	"github.com/gorizond/vcs-rag/internal/gitengine"
	"github.com/gorizond/vcs-rag/internal/provider/factory"
	"github.com/gorizond/vcs-rag/internal/store"
)

// Run собирает и запускает воркер: провайдер → эмбеддинги → store;
// размерность коллекции и индексы проверяются ДО первой записи (FR-009,
// fail-fast), затем параллельно работают линии discovery/mirror/index.
// Блокируется до отмены контекста.
func Run(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	// FR-014: второй воркер на том же MirrorsDir не запускается.
	release, err := AcquireLock(cfg.Worker.MirrorsDir)
	if err != nil {
		return err
	}
	defer release()

	vcs, err := factory.New(cfg)
	if err != nil {
		return err
	}
	emb, err := embed.New(cfg)
	if err != nil {
		return err
	}
	st, err := store.New(cfg.Qdrant)
	if err != nil {
		return err
	}
	defer st.Close()

	if err := st.EnsureCollection(ctx, uint64(cfg.Embeddings.Dim)); err != nil {
		return err
	}
	if err := st.EnsureIndexes(ctx); err != nil {
		return err
	}

	deps := RepoDeps{
		VCSType: vcs.Type(),
		VCS:     vcs,
		Engine:  gitengine.New(cfg.Worker.MirrorsDir),
		Embed:   emb,
		Store:   st,
		Cfg:     cfg,
		Logger:  logger,
	}
	snap := &snapshot{}

	var wg sync.WaitGroup
	errCh := make(chan error, 4)
	run := func(fn func() error) {
		defer wg.Done()
		errCh <- fn()
	}
	wg.Add(4)
	go run(func() error { return runDiscovery(ctx, vcs, cfg, logger, snap) })
	go run(func() error { return runMirror(ctx, cfg, deps.VCSType, deps.Engine, logger, snap) })
	go run(func() error { return runIndex(ctx, cfg, deps, snap) })
	go run(func() error { return runMetadata(ctx, cfg, deps, snap) })

	wg.Wait() // горутины завершаются по ctx.Done()
	close(errCh)
	for err := range errCh {
		if err != nil {
			return err
		}
	}
	logger.Info("worker остановлен")
	return nil
}
