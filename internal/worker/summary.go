package worker

import (
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// summary holds the counters of one lane pass (T060, FR-020): repositories,
// successes/errors, indexed commits, updates, resumed/skipped (T091), pruned,
// duration; plus the observability snapshot of the limiter/pacer (T131).
type summary struct {
	phase   string
	start   time.Time
	repos   atomic.Int64
	ok      atomic.Int64
	failed  atomic.Int64
	indexed atomic.Int64
	updated atomic.Int64 // metadata: items written this pass (T129 busy signal)
	resumed atomic.Int64 // index: commits already indexed before this pass
	skipped atomic.Int64 // metadata: items skipped as unchanged/already indexed
	pruned  atomic.Int64

	// a VCS rate limit seen in this pass (T129): the pacer dominates the delay.
	rateLimit  atomic.Bool
	retryAfter atomic.Int64 // nanoseconds

	// repo-GC counters (T140).
	gcKnown           atomic.Int64
	gcVanished        atomic.Int64
	gcDeletedRepos    atomic.Int64
	gcDeletedPoints   atomic.Int64
	gcSuppressedGrace atomic.Int64
	gcSuppressedBusy  atomic.Int64
	gcSuppressedCap   atomic.Int64
	gcAnomalies       atomic.Int64
	gcDryRun          atomic.Int64 // would-delete candidates in dry-run

	// metadata entity reconcile (T140).
	reconciled        atomic.Int64 // keys reaching the miss threshold
	goneKeys          atomic.Int64 // Meta keys dropped after a successful delete
	deletedEntities   atomic.Int64 // points deleted by ID
	metaSuppressedCap atomic.Int64 // keys not deleted because of a cap
	metaDeleteBudget  atomic.Int64 // CAS budget of deletions per pass

	// set by the run loop right before log (T131); single-goroutine by design.
	embed      *limiterStats
	paceDelay  time.Duration
	paceReason string
}

// metaDeletePerPass caps the entity deletions of one metadata pass (T140).
const metaDeletePerPass = 128

// newSummary starts a lane pass.
func newSummary(phase string) *summary {
	s := &summary{phase: phase, start: time.Now()}
	s.metaDeleteBudget.Store(metaDeletePerPass)
	return s
}

// reserveMetaDelete consumes one deletion slot of the pass budget (T140).
func (s *summary) reserveMetaDelete() bool {
	for {
		n := s.metaDeleteBudget.Load()
		if n <= 0 {
			return false
		}
		if s.metaDeleteBudget.CompareAndSwap(n, n-1) {
			return true
		}
	}
}

// countRepo records the processing result of one repository; a rate limit error
// is remembered for the pacer (T129).
func (s *summary) countRepo(err error) {
	s.repos.Add(1)
	if err != nil {
		s.failed.Add(1)
		if d, ok := provider.IsRateLimit(err); ok {
			s.markRateLimit(d)
		}
		return
	}
	s.ok.Add(1)
}

// addIndexed records indexed commits (the index lane).
func (s *summary) addIndexed(n int) { s.indexed.Add(int64(n)) }

// addUpdated records metadata items written (the metadata lane, T129).
func (s *summary) addUpdated(n int) { s.updated.Add(int64(n)) }

// addResumed records commits already indexed before this pass (the index lane, T091).
func (s *summary) addResumed(n int) { s.resumed.Add(int64(n)) }

// addSkipped records items skipped as unchanged/already indexed (the metadata lane, T091).
func (s *summary) addSkipped(n int) { s.skipped.Add(int64(n)) }

// addPruned records deleted points/commits (prune in index/metadata).
func (s *summary) addPruned(n int) { s.pruned.Add(int64(n)) }

// markRateLimit remembers a VCS rate limit (0 — the pacer falls back to 30s).
func (s *summary) markRateLimit(d time.Duration) {
	s.rateLimit.Store(true)
	if d > 0 {
		s.retryAfter.Store(int64(d))
	}
}

// rateLimited reports the rate limit state of the pass.
func (s *summary) rateLimited() (bool, time.Duration) {
	return s.rateLimit.Load(), time.Duration(s.retryAfter.Load())
}

// withEmbed attaches the limiter snapshot (T131).
func (s *summary) withEmbed(st limiterStats) { s.embed = &st }

// withPace attaches the computed pace of the next pass (T131).
func (s *summary) withPace(d time.Duration, reason string) {
	s.paceDelay, s.paceReason = d, reason
}

// log writes the single pass summary (FR-020); it does not replace existing logs.
func (s *summary) log(logger *slog.Logger) {
	attrs := []any{
		"repos", s.repos.Load(),
		"ok", s.ok.Load(),
		"errors", s.failed.Load(),
		"indexed_commits", s.indexed.Load(),
		"updated", s.updated.Load(),
		"resumed", s.resumed.Load(),
		"skipped", s.skipped.Load(),
		"pruned", s.pruned.Load(),
	}
	if s.phase == "gc" {
		attrs = append(attrs,
			"gc_known", s.gcKnown.Load(),
			"gc_vanished", s.gcVanished.Load(),
			"gc_deleted_repos", s.gcDeletedRepos.Load(),
			"gc_deleted_points", s.gcDeletedPoints.Load(),
			"gc_suppressed_grace", s.gcSuppressedGrace.Load(),
			"gc_suppressed_busy", s.gcSuppressedBusy.Load(),
			"gc_suppressed_cap", s.gcSuppressedCap.Load(),
			"gc_anomaly", s.gcAnomalies.Load(),
			"gc_dry_run", s.gcDryRun.Load(),
		)
	}
	if s.phase == "metadata" {
		attrs = append(attrs,
			"reconciled", s.reconciled.Load(),
			"gone_keys", s.goneKeys.Load(),
			"deleted_entities", s.deletedEntities.Load(),
			"suppressed_cap", s.metaSuppressedCap.Load(),
		)
	}
	if s.embed != nil {
		attrs = append(attrs,
			"embed_cap", s.embed.Limit,
			"embed_reason", s.embed.Reason,
			"embed_p50", s.embed.P50.Round(time.Millisecond).String(),
			"embed_p95", s.embed.P95.Round(time.Millisecond).String(),
			"embed_throttles", s.embed.Throttles,
		)
	}
	if s.paceReason != "" {
		attrs = append(attrs,
			"pace_delay", s.paceDelay.Round(time.Millisecond).String(),
			"pace_reason", s.paceReason,
		)
	}
	attrs = append(attrs, "duration", time.Since(s.start).Round(time.Millisecond).String())
	logger.Info("pass summary: "+s.phase, attrs...)
}
