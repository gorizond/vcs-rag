// Package worker provides background lanes: discovery, mirror, index (FR-005/006/018).
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

// Run assembles and starts the worker: provider → embeddings → store;
// the collection dimension and indexes are checked BEFORE the first write
// (FR-009, fail-fast), then the discovery/mirror/index lanes run in parallel.
// Blocks until context cancellation.
func Run(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	// FR-014: a second worker on the same MirrorsDir will not start.
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

	// T074: private repositories — auth env for network git operations;
	// the token never reaches the clone URL or the mirror config.
	engine := gitengine.New(cfg.Worker.MirrorsDir)
	engine.AuthEnv = gitengine.AuthEnv(vcs.Type(), providerToken(cfg, vcs.Type()))

	deps := RepoDeps{
		VCSType: vcs.Type(),
		VCS:     vcs,
		Engine:  engine,
		// T128: every worker-side embeddings call (lanes + refresh) goes through
		// the AIMD limiter; the ceiling is WORKER_CONCURRENCY.
		Embed:  newLimiter(emb, cfg.Worker.Concurrency, logger),
		Store:  st,
		Cfg:    cfg,
		Logger: logger,
	}
	snap := &snapshot{}

	var wg sync.WaitGroup
	errCh := make(chan error, 5)
	run := func(fn func() error) {
		defer wg.Done()
		errCh <- fn()
	}
	wg.Add(4)
	go run(func() error { return runDiscovery(ctx, vcs, cfg, logger, snap) })
	go run(func() error { return runMirror(ctx, cfg, deps.VCSType, deps.Engine, logger, snap) })
	go run(func() error { return runIndex(ctx, cfg, deps, snap) })
	go run(func() error { return runMetadata(ctx, cfg, deps, snap) })

	// T108/T112: the on-demand refresh over the local JetStream; NATS_URL is
	// empty by default — the behavior stays as before.
	if cfg.NATS.URL != "" {
		wg.Add(1)
		go run(func() error { return runRefreshLane(ctx, cfg, deps, snap, logger) })
	}

	wg.Wait() // goroutines exit on ctx.Done()
	close(errCh)
	for err := range errCh {
		if err != nil {
			return err
		}
	}
	logger.Info("worker stopped")
	return nil
}

// providerToken returns the active provider token from the config (for git auth).
func providerToken(cfg *config.Config, vcsType string) string {
	if vcsType == "github" {
		return cfg.Provider.GitHubToken
	}
	return cfg.Provider.GitLabToken
}
