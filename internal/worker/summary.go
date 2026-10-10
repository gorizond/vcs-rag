package worker

import (
	"log/slog"
	"sync/atomic"
	"time"
)

// summary holds the counters of one lane pass (T060, FR-020): repositories,
// successes/errors, indexed commits, resumed/skipped (T091), pruned, duration.
type summary struct {
	phase   string
	start   time.Time
	repos   atomic.Int64
	ok      atomic.Int64
	failed  atomic.Int64
	indexed atomic.Int64
	resumed atomic.Int64 // index: commits already indexed before this pass
	skipped atomic.Int64 // metadata: items skipped as unchanged/already indexed
	pruned  atomic.Int64
}

// newSummary starts a lane pass.
func newSummary(phase string) *summary {
	return &summary{phase: phase, start: time.Now()}
}

// countRepo records the processing result of one repository.
func (s *summary) countRepo(err error) {
	s.repos.Add(1)
	if err != nil {
		s.failed.Add(1)
		return
	}
	s.ok.Add(1)
}

// addIndexed records indexed commits (the index lane).
func (s *summary) addIndexed(n int) { s.indexed.Add(int64(n)) }

// addResumed records commits already indexed before this pass (the index lane, T091).
func (s *summary) addResumed(n int) { s.resumed.Add(int64(n)) }

// addSkipped records items skipped as unchanged/already indexed (the metadata lane, T091).
func (s *summary) addSkipped(n int) { s.skipped.Add(int64(n)) }

// addPruned records deleted points/commits (prune in index/metadata).
func (s *summary) addPruned(n int) { s.pruned.Add(int64(n)) }

// log writes the single pass summary (FR-020); it does not replace existing logs.
func (s *summary) log(logger *slog.Logger) {
	logger.Info("pass summary: "+s.phase,
		"repos", s.repos.Load(),
		"ok", s.ok.Load(),
		"errors", s.failed.Load(),
		"indexed_commits", s.indexed.Load(),
		"resumed", s.resumed.Load(),
		"skipped", s.skipped.Load(),
		"pruned", s.pruned.Load(),
		"duration", time.Since(s.start).Round(time.Millisecond).String(),
	)
}
