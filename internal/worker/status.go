package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/gorizond/vcs-rag/internal/config"
	"github.com/gorizond/vcs-rag/internal/gitengine"
)

// Worker indexing status (T150). At most once per minute the index lane
// publishes a snapshot into the instance KV bucket worker_status_<id> (key
// "status", TTL 24h) so the MCP index_status tool can answer without touching
// the index itself. Publishing is best-effort and never affects the lanes:
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

// publishIndexStatus publishes the status when the throttle allows (T150);
// a nil publisher (NATS off) keeps the lane quiet.
func publishIndexStatus(ctx context.Context, p *statusPublisher, deps RepoDeps, snap *snapshot, sum *summary) {
	if p == nil || !p.due() {
		return
	}
	p.publish(collectWorkerStatus(ctx, deps, snap, sum, deps.Store, p.now()))
}

// collectWorkerStatus builds the status from the discovery snapshot and the
// per-repository state sidecars (T150). The reads are small and bounded by the
// repository count; the points count is one exact call (skipped on error).
// A repository counts as indexed when its state has at least one branch with
// at least one indexed commit.
func collectWorkerStatus(ctx context.Context, deps RepoDeps, snap *snapshot, sum *summary, counter statusPoints, now time.Time) *workerStatus {
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
	if sum != nil {
		st.LastPass = &indexPassStats{
			Repos:          sum.repos.Load(),
			OK:             sum.ok.Load(),
			Errors:         sum.failed.Load(),
			IndexedCommits: sum.indexed.Load(),
			Resumed:        sum.resumed.Load(),
			Skipped:        sum.skipped.Load(),
			Pruned:         sum.pruned.Load(),
			DurationMS:     time.Since(sum.start).Milliseconds(),
		}
	}
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
