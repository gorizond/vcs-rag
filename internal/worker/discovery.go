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
	stamp time.Time // time of the last successful listing (T140 GC staleness)
}

func (s *snapshot) set(repos []provider.Repo) {
	s.mu.Lock()
	s.repos = repos
	s.stamp = time.Now()
	s.mu.Unlock()
}

func (s *snapshot) get() []provider.Repo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.repos
}

// at returns the time of the last successful listing (zero — never listed);
// the GC lane freezes on a stale snapshot (T140).
func (s *snapshot) at() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stamp
}

// runDiscovery is the discovery lane: periodic repository listing
// (WORKER_INTERVAL), optionally cached for WORKER_DISCOVERY_MIN_INTERVAL
// (T143). A listing error does not break the loop (FR-014).
func runDiscovery(ctx context.Context, p provider.VCSProvider, cfg *config.Config, logger *slog.Logger, snap *snapshot) error {
	logger = log.WithPhase(logger, "discovery")
	listOnceCached(ctx, p, cfg, logger, snap)

	ticker := time.NewTicker(cfg.Worker.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			listOnceCached(ctx, p, cfg, logger, snap)
		}
	}
}

// listOnceCached reuses a snapshot younger than WORKER_DISCOVERY_MIN_INTERVAL
// without an API call (T143); a zero interval always refreshes (the previous
// behavior).
func listOnceCached(ctx context.Context, p provider.VCSProvider, cfg *config.Config, logger *slog.Logger, snap *snapshot) {
	minInterval := cfg.Worker.DiscoveryMinInterval
	if minInterval > 0 {
		if at := snap.at(); !at.IsZero() && time.Since(at) < minInterval {
			logger.Debug("discovery: cached", "age", time.Since(at).Round(time.Second).String(),
				"min_interval", minInterval.String())
			return
		}
	}
	listOnce(ctx, p, cfg, logger, snap)
}

// listOnce performs one pass: listing, a log line per repository,
// snapshot update. A rate limit is transient and is waited out with the same
// FR-007 policy as the per-unit work (RetryFloor…RetryCap, MaxRetries retries);
// other errors and exhausted retries are logged once (FR-001).
func listOnce(ctx context.Context, p provider.VCSProvider, cfg *config.Config, logger *slog.Logger, snap *snapshot) {
	policy := retryPolicyOf(passConfig(cfg))
	pace := newPacerWithLimits(cfg.Worker.Interval, policy.floor, policy.cap)
	var repos []provider.Repo
	for attempt := 0; ; attempt++ {
		var err error
		repos, err = p.ListRepositories(ctx)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return // normal shutdown — stay quiet
		}
		retryAfter, limited := provider.IsRateLimit(err)
		if !limited {
			logger.Error("repository listing failed", "err", err)
			return
		}
		if attempt >= policy.maxRetries {
			logger.Error("repository listing failed: rate limit retries exhausted",
				"err", err, "attempts", attempt+1)
			return
		}
		wait := pace.next(false, true, retryAfter)
		logger.Warn("repository listing rate limited: pausing before retry",
			"err", err, "retry_after", retryAfter.String(), "wait", wait.String(), "attempt", attempt+1)
		if sleepCtx(ctx, wait) != nil {
			return // normal shutdown during the pause
		}
	}
	snap.set(repos)
	logger.Info("repositories fetched", "provider", p.Type(), "count", len(repos))
	for _, r := range repos {
		log.WithRepo(logger, r.Namespace+"/"+r.Path).Info("repo", "namespace", r.Namespace)
	}
}
