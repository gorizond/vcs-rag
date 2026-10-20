package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/gorizond/vcs-rag/internal/config"
	"github.com/gorizond/vcs-rag/internal/gitengine"
	"github.com/gorizond/vcs-rag/internal/provider"
)

// Worker indexing status (T150). At most once per minute the worker publishes
// a snapshot into the instance KV bucket worker_status_<id> (key "status", TTL
// 24h) so the MCP index_status tool can answer without touching the index
// itself. Since T153 the publisher is a standalone ticker (statusReporter),
// not a pass-end hook: a long backfill pass no longer freezes the published
// snapshot, and an empty discovery snapshot never overwrites the previous
// value with zeros. Publishing is best-effort and never affects the lanes:
// NATS off or down keeps the previous behavior (complete silence). The
// refresh_status_<id> contract, subjects and consumers are untouched.
const (
	workerStatusKey       = "status"
	workerStatusTTL       = 24 * time.Hour
	workerStatusInterval  = time.Minute
	workerStatusSampleMax = 10
)

// indexPassStats mirrors the counters of one index pass.
type indexPassStats struct {
	Repos          int64 `json:"repos"`
	OK             int64 `json:"ok"`
	Errors         int64 `json:"errors"`
	IndexedCommits int64 `json:"indexed_commits"`
	Resumed        int64 `json:"resumed"`
	Skipped        int64 `json:"skipped"`
	Pruned         int64 `json:"pruned"`
	DurationMS     int64 `json:"duration_ms"`
	// US2 budget report (FR-003/FR-010/FR-015). The remaining US4 counters
	// (rate_limited_*, phase) are owned by that story.
	Unlimited         bool     `json:"unlimited,omitempty"`
	BudgetExhausted   bool     `json:"budget_exhausted,omitempty"`
	UnprocessedSample []string `json:"unprocessed_sample,omitempty"`
	SharedAccountWarn bool     `json:"shared_account_warn,omitempty"`
	// US4 rate-limit report (FR-008/FR-009/FR-012), all omitempty.
	RateLimitedCount int64    `json:"rate_limited_count,omitempty"`
	RateLimitedRepos int64    `json:"rate_limited_repos,omitempty"`
	LastRetryAfterMS int64    `json:"last_retry_after_ms,omitempty"`
	RatePauseUntil   string   `json:"rate_pause_until,omitempty"`
	Phase            string   `json:"phase,omitempty"`
	ExcludedSample   []string `json:"excluded_sample,omitempty"`
}

// upstreamCounter is the instance-level upstream counter (FR-008): the request
// window, 429 events, the last Retry-After and the current phase. The pass
// report carries the per-pass deltas; the counter keeps the instance totals.
type upstreamCounter struct {
	requestsWindow atomic.Int64
	four29s        atomic.Int64
	lastRetryAfter atomic.Int64 // nanoseconds
	phase          atomic.Value // string
}

// setPhase records the current phase (active|rate_pause|budget_exhausted|idle|error).
func (c *upstreamCounter) setPhase(p string) { c.phase.Store(p) }

// phaseName returns the current phase ("" before the first pass).
func (c *upstreamCounter) phaseName() string {
	if v, ok := c.phase.Load().(string); ok {
		return v
	}
	return ""
}

// workerStatus is the published payload (the MCP read model repeats it in
// internal/mcp/tools.go — the refreshStatus precedent).
type workerStatus struct {
	ReposTotal    int             `json:"repos_total"`
	ReposIndexed  int             `json:"repos_indexed"`
	ReposPending  int             `json:"repos_pending"`
	PendingSample []string        `json:"pending_sample"`
	Points        *uint64         `json:"points,omitempty"` // nil — the count failed
	LastPass      *indexPassStats `json:"last_pass,omitempty"`
	UpdatedAt     string          `json:"updated_at"`
}

// statusStore is the KV surface of the publisher (fake in tests).
type statusStore interface {
	Put(key string, value []byte) error
}

// statusPoints is the read-back surface for the points count (fake in tests).
type statusPoints interface {
	Count(ctx context.Context) (uint64, error)
}

// statusPublisher throttles the status writes; nil — publishing is off.
type statusPublisher struct {
	store    statusStore
	logger   *slog.Logger
	now      func() time.Time
	interval time.Duration
	last     time.Time
	nc       *nats.Conn // owned connection in production (nil in tests)
}

// newStatusPublisher creates the throttled publisher (one write per minute).
func newStatusPublisher(store statusStore, logger *slog.Logger) *statusPublisher {
	return &statusPublisher{store: store, logger: logger, now: time.Now, interval: workerStatusInterval}
}

// Close closes the owned NATS connection.
func (p *statusPublisher) Close() {
	if p.nc != nil {
		p.nc.Close()
	}
}

// due reports whether the interval passed since the last publication.
func (p *statusPublisher) due() bool {
	now := p.now()
	if !p.last.IsZero() && now.Sub(p.last) < p.interval {
		return false
	}
	p.last = now
	return true
}

// publish writes the snapshot; failures are logged, never fatal.
func (p *statusPublisher) publish(st *workerStatus) {
	data, err := json.Marshal(st)
	if err != nil {
		p.logger.Warn("worker status marshal failed", "err", err)
		return
	}
	if err := p.store.Put(workerStatusKey, data); err != nil {
		p.logger.Warn("worker status publish failed", "err", err)
	}
}

// statusReporter periodically publishes the indexing status (T153): a
// standalone ticker instead of the pass-end hook, so the published snapshot
// stays fresh during a long pass. The index lane only records its last
// COMPLETED pass; the reporter goroutine is the single writer of the KV.
type statusReporter struct {
	pub    *statusPublisher
	deps   RepoDeps
	snap   *snapshot
	points statusPoints // deps.Store in production (narrow interface for tests)

	counter upstreamCounter // FR-008 instance upstream totals
	// per-pass deltas of the provider totals (single writer: the index lane).
	lastPages int64
	last429s  int64

	mu          sync.Mutex
	lastPass    *indexPassStats // nil before the first completed pass
	emptyLogged bool            // one debug line per empty-snapshot episode
}

// newStatusReporter creates the reporter; nil when the publisher is nil
// (NATS off — the lanes stay quiet).
func newStatusReporter(pub *statusPublisher, deps RepoDeps, snap *snapshot) *statusReporter {
	if pub == nil {
		return nil
	}
	return &statusReporter{pub: pub, deps: deps, snap: snap, points: deps.Store}
}

// recordPass stores the counters of a completed index pass; before the first
// completion last_pass stays omitted. Nil-safe: without NATS the index lane
// has no reporter.
func (r *statusReporter) recordPass(sum *summary) {
	if r == nil || sum == nil {
		return
	}
	st := passStats(sum)
	// FR-008: merge the provider-side observation (API pages and 429 responses)
	// into the per-pass report; the summary already counted git-side 429s.
	if pc, ok := r.deps.VCS.(provider.PageCounter); ok {
		total := pc.Pages()
		if d := total - r.lastPages; d > 0 {
			r.counter.requestsWindow.Add(d)
		}
		r.lastPages = total
	}
	if rc, ok := r.deps.VCS.(provider.RateLimitEventCounter); ok {
		total := rc.RateLimitEvents()
		if d := total - r.last429s; d > 0 {
			st.RateLimitedCount += d
			r.counter.four29s.Add(d)
		}
		r.last429s = total
	}
	if st.LastRetryAfterMS == 0 {
		if ra, ok := r.deps.VCS.(provider.RetryAfterReporter); ok {
			if d := ra.LastRetryAfter(); d > 0 {
				st.LastRetryAfterMS = d.Milliseconds()
			}
		}
	}
	if st.LastRetryAfterMS > 0 {
		r.counter.lastRetryAfter.Store(int64(time.Duration(st.LastRetryAfterMS) * time.Millisecond))
	}
	// The provider deltas are part of this pass: derive the phase from the
	// merged total, not only from the git-side summary count.
	st.Phase = passPhase(sum, st.RateLimitedCount)
	r.counter.setPhase(st.Phase)
	if r.deps.Cfg != nil && r.deps.Cfg.Worker.SharedAccountWarn {
		if used, ok := sharedAccountUsage(r.deps.VCS); ok && used > sharedAccountWarnThreshold {
			st.SharedAccountWarn = true
			if r.deps.Logger != nil {
				r.deps.Logger.Warn("shared account budget above 80%: coordinate the instances",
					"used", used, "reference", "Free sustained 5000/h")
			}
		}
	}
	r.mu.Lock()
	r.lastPass = st
	r.mu.Unlock()
}

// run publishes the status immediately and then once per interval until ctx is
// done; it starts together with the other lanes.
func (r *statusReporter) run(ctx context.Context) error {
	ticker := time.NewTicker(r.pub.interval)
	defer ticker.Stop()
	r.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.tick(ctx)
		}
	}
}

// tick publishes one snapshot when the throttle allows. An empty discovery
// snapshot is skipped (debug once per episode): the previous value must not be
// overwritten with zeros while the discovery lane is still starting.
func (r *statusReporter) tick(ctx context.Context) {
	if len(r.snap.get()) == 0 {
		if !r.emptyLogged {
			r.emptyLogged = true
			r.deps.Logger.Debug("worker status deferred: the discovery snapshot is empty")
		}
		return
	}
	r.emptyLogged = false
	if !r.pub.due() {
		return
	}
	r.mu.Lock()
	last := r.lastPass
	r.mu.Unlock()
	r.pub.publish(collectWorkerStatus(ctx, r.deps, r.snap, last, r.points, r.pub.now()))
}

// passStats snapshots the counters of one completed pass.
func passStats(sum *summary) *indexPassStats {
	st := &indexPassStats{
		Repos:             sum.repos.Load(),
		OK:                sum.ok.Load(),
		Errors:            sum.failed.Load(),
		IndexedCommits:    sum.indexed.Load(),
		Resumed:           sum.resumed.Load(),
		Skipped:           sum.skipped.Load(),
		Pruned:            sum.pruned.Load(),
		DurationMS:        time.Since(sum.start).Milliseconds(),
		Unlimited:         sum.unlimited.Load(),
		BudgetExhausted:   sum.budgetExhausted.Load(),
		UnprocessedSample: sum.unprocessedSample(),
		RateLimitedCount:  sum.rateCount.Load(),
		RateLimitedRepos:  sum.rateRepos.Load(),
		LastRetryAfterMS:  time.Duration(sum.retryAfter.Load()).Milliseconds(),
		ExcludedSample:    sum.excludedSample(),
	}
	if until := sum.ratePauseUntil(); !until.IsZero() {
		st.RatePauseUntil = until.UTC().Format(time.RFC3339)
	}
	st.Phase = passPhase(sum, st.RateLimitedCount)
	return st
}

// passPhase derives the pass state-machine phase (contracts/status-report.md).
func passPhase(sum *summary, rateCount int64) string {
	switch {
	case sum.budgetExhausted.Load():
		return "budget_exhausted"
	case rateCount > 0 || sum.rateLimit.Load():
		return "rate_pause"
	case sum.repos.Load() == 0:
		return "idle"
	case sum.failed.Load() > 0 && sum.ok.Load() == 0:
		return "error"
	default:
		return "active"
	}
}

// sharedAccountWarnThreshold is the FR-015 soft warning level (80%).
const sharedAccountWarnThreshold = 0.8

// freeSustainedPerHour is the GitLab.com Free authenticated sustained budget;
// the header-less fallback compares the instance page count against it.
const freeSustainedPerHour = 5000

// sharedAccountUsage reports the authenticated account budget consumption:
// RateLimit-* headers when the provider exposes them, otherwise the instance
// page count against the Free sustained reference (FR-015). Soft coordination
// only — there is no shared counter between instances.
func sharedAccountUsage(v provider.VCSProvider) (float64, bool) {
	if obs, ok := v.(provider.RateLimitObserver); ok {
		if used, ok := obs.RateUsage(); ok {
			return used, true
		}
	}
	if pc, ok := v.(provider.PageCounter); ok {
		return float64(pc.Pages()) / freeSustainedPerHour, true
	}
	return 0, false
}

// collectWorkerStatus builds the status from the discovery snapshot and the
// per-repository state sidecars (T150). The reads are small and bounded by the
// repository count; the points count is one exact call (skipped on error).
// A repository counts as indexed when its state has at least one branch with
// at least one indexed commit. lastPass is the last completed pass (nil —
// omitted).
func collectWorkerStatus(ctx context.Context, deps RepoDeps, snap *snapshot, lastPass *indexPassStats, counter statusPoints, now time.Time) *workerStatus {
	repos := snap.get()
	st := &workerStatus{
		ReposTotal:    len(repos),
		PendingSample: []string{},
		UpdatedAt:     now.UTC().Format(time.RFC3339),
	}
	var pending []string
	for _, repo := range repos {
		state, err := LoadState(gitengine.StatePath(deps.Engine.MirrorPath(deps.VCSType, repo)))
		if err == nil && indexedState(state) {
			st.ReposIndexed++
			continue
		}
		pending = append(pending, repo.Namespace+"/"+repo.Path)
	}
	sort.Strings(pending)
	st.ReposPending = len(pending)
	if len(pending) > workerStatusSampleMax {
		pending = pending[:workerStatusSampleMax]
	}
	st.PendingSample = append(st.PendingSample, pending...)
	if counter != nil {
		if n, err := counter.Count(ctx); err == nil {
			st.Points = &n
		}
	}
	st.LastPass = lastPass
	return st
}

// indexedState reports whether the state has at least one indexed commit.
func indexedState(st *MirrorState) bool {
	for _, branch := range st.Branches {
		if len(branch.IndexedCommits) > 0 {
			return true
		}
	}
	return false
}

// natsStatusStore lazily ensures the worker status KV bucket and writes into
// it: reconnects are handled by the connection, a failed ensure is retried on
// the next publication.
type natsStatusStore struct {
	nc    *nats.Conn
	names config.RefreshTopology

	mu sync.Mutex
	kv nats.KeyValue
}

// Put ensures the bucket once and writes the entry.
func (s *natsStatusStore) Put(key string, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.kv == nil {
		js, err := s.nc.JetStream()
		if err != nil {
			return fmt.Errorf("nats: jetstream: %w", err)
		}
		kv, err := ensureWorkerStatusKV(js, s.names)
		if err != nil {
			return err
		}
		s.kv = kv
	}
	// reuse the refresh KV adapter (nats.go): it drops the revision.
	return natsKV{s.kv}.Put(key, value)
}

// ensureWorkerStatusKV returns the instance status bucket, creating it (TTL
// 24h) when missing — the create-if-missing pattern of the refresh topology.
func ensureWorkerStatusKV(js nats.JetStreamContext, names config.RefreshTopology) (nats.KeyValue, error) {
	kv, err := js.KeyValue(names.WorkerKVBucket)
	switch {
	case err == nil:
		return kv, nil
	case errors.Is(err, nats.ErrBucketNotFound):
		kv, err := js.CreateKeyValue(&nats.KeyValueConfig{
			Bucket: names.WorkerKVBucket, TTL: workerStatusTTL, Storage: nats.FileStorage,
		})
		if err != nil {
			return nil, fmt.Errorf("nats: create KV %s: %w", names.WorkerKVBucket, err)
		}
		return kv, nil
	default:
		return nil, fmt.Errorf("nats: KV %s: %w", names.WorkerKVBucket, err)
	}
}

// connectWorkerStatus dials NATS for the status publisher (T150); reconnect is
// perpetual and the bucket is ensured lazily on the first publication.
func connectWorkerStatus(url string, names config.RefreshTopology, logger *slog.Logger) (*statusPublisher, error) {
	nc, err := nats.Connect(url,
		nats.Name(names.ConnName+"-status"),
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("nats: connect: %w", err)
	}
	p := newStatusPublisher(&natsStatusStore{nc: nc, names: names}, logger)
	p.nc = nc
	return p, nil
}
