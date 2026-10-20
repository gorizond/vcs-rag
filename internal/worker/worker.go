// Package worker provides background lanes: discovery, mirror, index (FR-005/006/018).
package worker

import (
	"context"
	"log/slog"
	"sync"

	"github.com/gorizond/vcs-rag/internal/config"
	"github.com/gorizond/vcs-rag/internal/embed"
	"github.com/gorizond/vcs-rag/internal/gitengine"
	"github.com/gorizond/vcs-rag/internal/provider"
	"github.com/gorizond/vcs-rag/internal/provider/factory"
	"github.com/gorizond/vcs-rag/internal/store"
)

// Run assembles and starts the worker: provider → embeddings → store;
// the collection dimension and indexes are checked BEFORE the first write
// (FR-009, fail-fast), then the discovery/mirror/index/graph lanes run in
// parallel. Blocks until context cancellation.
func Run(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	// FR-014: a second worker on the same MirrorsDir will not start.
	release, err := AcquireLock(cfg.Worker.MirrorsDir)
	if err != nil {
		return err
	}
	defer release()

	// FR-002: one instance-wide upstream gate + bounded queue shared by all
	// lanes (git clone/fetch and API traffic).
	gate, queue := upstreamFor(cfg)
	vcs, err := factory.New(cfg)
	if err != nil {
		return err
	}
	if s, ok := vcs.(interface{ SetUpstreamGate(*provider.UpstreamGate) }); ok {
		s.SetUpstreamGate(gate)
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

	// T010: the graph store shares the vector client but owns its payload-only
	// collection (instance-scoped QDRANT_GRAPH_COLLECTION); the graph lane
	// bootstraps it fail-soft — a failure disables only that lane.
	graphSt := store.NewGraphStore(st, cfg.Qdrant.GraphCollection)

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

		Upstream: gate,
		Queue:    queue,
	}
	snap := &snapshot{}

	// T150/T153: the indexing status reporter — only with NATS_URL set; a
	// failed connection disables the status, never the worker. The reporter
	// publishes on its own ticker, so a long pass does not freeze the snapshot.
	var statusRep *statusReporter
	if cfg.NATS.URL != "" {
		names := config.RefreshNames(cfg.NATS.InstanceID)
		sp, err := connectWorkerStatus(cfg.NATS.URL, names, logger)
		if err != nil {
			logger.Warn("worker status disabled", "err", err)
		} else {
			defer sp.Close()
			statusRep = newStatusReporter(sp, deps, snap)
		}
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 8)
	run := func(fn func() error) {
		defer wg.Done()
		errCh <- fn()
	}
	wg.Add(6)
	go run(func() error { return runDiscovery(ctx, vcs, cfg, logger, snap) })
	go run(func() error { return runMirror(ctx, cfg, deps, logger, snap) })
	go run(func() error { return runIndex(ctx, cfg, deps, snap, statusRep) })
	go run(func() error { return runMetadata(ctx, cfg, deps, snap) })
	// T140: repo-GC — discovery is the source of truth; dry-run by default.
	go run(func() error { return runGC(ctx, cfg, deps, snap) })
	// T010: the SDLC graph lane — CPU-only, its own pacer/lock, no LM budget (R13).
	go run(func() error { return runGraph(ctx, cfg, deps, snap, graphSt) })
	if statusRep != nil {
		wg.Add(1)
		go run(func() error { return statusRep.run(ctx) })
	}

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
