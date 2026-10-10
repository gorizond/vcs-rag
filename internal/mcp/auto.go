package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

// Auto-refresh policy (T134): before a repo-scoped search the MCP server
// itself publishes a refresh task when the index looks stale, waits at most
// autoWaitMax for the correlated result and always continues the search.
// Anti-DDoS budgets: a per-repo interval and fixed-window per-minute/per-hour
// publication buckets. NATS off (no auto client) keeps the behavior as before.
const (
	autoWaitMax         = 3 * time.Second  // max extra latency per search
	autoDoneFresh       = 10 * time.Minute // a newer "done" is fresh enough
	autoInProgressFresh = 15 * time.Minute // a younger "in_progress" is awaited, not republished
	autoRepoInterval    = 5 * time.Minute  // per-repo publication interval
	autoBucketPerMinute = 6                // global fixed-window budget
	autoBucketPerHour   = 60
	autoJitter          = 300 * time.Millisecond // U[0, autoJitter] pause before publishing
	autoLogPeriod       = 5 * time.Minute        // INFO summary of the cumulative counters
)

// autoRefresher is the narrow client surface used by the policy (implemented
// by *natsRefresher; fakes in tests).
type autoRefresher interface {
	// Status reads the current KV status of the repository.
	Status(repo string) (refreshStatus, bool)
	// Wait polls up to wait for a terminal status: want=="" accepts any
	// terminal status, otherwise it must carry that request id. Reports
	// whether a terminal status was observed.
	Wait(ctx context.Context, repo, want string, wait time.Duration) bool
	// Trigger publishes a refresh task with origin="auto" and returns its id.
	Trigger(repo string) (string, error)
}

// autoStats are the cumulative decision counters (logged as a 5m summary).
type autoStats struct {
	checked             uint64
	published           uint64
	fresh               uint64
	suppressedInterval  uint64
	suppressedBucket    uint64
	suppressedAmbiguous uint64
	waitDone            uint64
	waitTimeout         uint64
}

// autoClient drives the auto-refresh decisions for one MCP server instance.
type autoClient struct {
	ref    autoRefresher
	logger *slog.Logger
	now    func() time.Time
	rnd    func() float64 // U[0,1), publication jitter
	jitter time.Duration

	mu            sync.Mutex
	last          map[string]time.Time // repo → last publication
	minuteAt      time.Time
	minuteN       int
	hourAt        time.Time
	hourN         int
	stats         autoStats
	lastLog       time.Time
	loggedChecked uint64
}

// newAutoClient creates the policy driver; nil logger means slog.Default().
func newAutoClient(ref autoRefresher, logger *slog.Logger) *autoClient {
	if logger == nil {
		logger = slog.Default()
	}
	return &autoClient{
		ref:     ref,
		logger:  logger,
		now:     time.Now,
		rnd:     rand.Float64,
		jitter:  autoJitter,
		last:    make(map[string]time.Time),
		lastLog: time.Now(),
	}
}

// beforeSearch decides whether one repository needs an auto-refresh and, when
// allowed, publishes it and waits for the correlated result. Best-effort: any
// suppression or failure leaves the search untouched; the extra latency is
// capped at autoWaitMax.
func (a *autoClient) beforeSearch(ctx context.Context, raw string, candidates []string) {
	a.record(func(s *autoStats) { s.checked++ })

	if len(candidates) != 1 {
		a.record(func(s *autoStats) { s.suppressedAmbiguous++ })
		a.logger.Debug("auto-refresh skipped", "repo", raw, "reason", "ambiguous_or_not_found", "candidates", len(candidates))
		return
	}
	repo := candidates[0]
	start := time.Now()

	if st, ok := a.ref.Status(repo); ok {
		now := a.now()
		switch {
		case st.State == "done" && now.Sub(st.FinishedAt) < autoDoneFresh:
			a.record(func(s *autoStats) { s.fresh++ })
			return
		case st.State == "in_progress" && now.Sub(st.StartedAt) < autoInProgressFresh:
			// a live run: wait for it without publishing anything.
			a.wait(ctx, repo, "", start)
			return
		}
	}

	if !a.allowRepo(repo) {
		a.record(func(s *autoStats) { s.suppressedInterval++ })
		a.logger.Debug("auto-refresh skipped", "repo", repo, "reason", "interval")
		return
	}
	if !a.allowBucket() {
		a.record(func(s *autoStats) { s.suppressedBucket++ })
		a.logger.Debug("auto-refresh skipped", "repo", repo, "reason", "bucket")
		return
	}

	if delay := time.Duration(a.rnd() * float64(a.jitter)); delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return
		}
	}

	id, err := a.ref.Trigger(repo)
	if err != nil {
		a.logger.Warn("auto-refresh publish failed", "repo", repo, "err", err)
		return
	}
	a.record(func(s *autoStats) { s.published++ })
	a.noteRepo(repo)
	a.wait(ctx, repo, id, start)
}

// wait polls for the terminal status within the remaining latency budget
// (autoWaitMax minus the time already spent in beforeSearch).
func (a *autoClient) wait(ctx context.Context, repo, want string, start time.Time) {
	budget := autoWaitMax - time.Since(start)
	if budget < 0 {
		budget = 0
	}
	if a.ref.Wait(ctx, repo, want, budget) {
		a.record(func(s *autoStats) { s.waitDone++ })
	} else {
		a.record(func(s *autoStats) { s.waitTimeout++ })
	}
}

// allowRepo applies the per-repo publication interval. Two concurrent searches
// of one repo may both pass (the publish happens outside the lock) — the
// worker coalesces such duplicates.
func (a *autoClient) allowRepo(repo string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if last, ok := a.last[repo]; ok && a.now().Sub(last) < autoRepoInterval {
		return false
	}
	return true
}

// noteRepo records a successful publication.
func (a *autoClient) noteRepo(repo string) {
	a.mu.Lock()
	a.last[repo] = a.now()
	a.mu.Unlock()
}

// allowBucket consumes one token of the fixed-window minute/hour budgets.
func (a *autoClient) allowBucket() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	if a.minuteAt.IsZero() || now.Sub(a.minuteAt) >= time.Minute {
		a.minuteAt, a.minuteN = now, 0
	}
	if a.hourAt.IsZero() || now.Sub(a.hourAt) >= time.Hour {
		a.hourAt, a.hourN = now, 0
	}
	if a.minuteN >= autoBucketPerMinute || a.hourN >= autoBucketPerHour {
		return false
	}
	a.minuteN++
	a.hourN++
	return true
}

// record updates a counter and emits the cumulative INFO summary at most once
// per autoLogPeriod (and only when something was checked since the last one).
func (a *autoClient) record(f func(*autoStats)) {
	a.mu.Lock()
	f(&a.stats)
	now := a.now()
	if now.Sub(a.lastLog) >= autoLogPeriod && a.stats.checked > a.loggedChecked {
		a.lastLog = now
		a.loggedChecked = a.stats.checked
		st := a.stats
		a.mu.Unlock()
		a.logger.Info("auto-refresh summary",
			"checked", st.checked, "published", st.published, "fresh", st.fresh,
			"suppressed_interval", st.suppressedInterval, "suppressed_bucket", st.suppressedBucket,
			"suppressed_ambiguous", st.suppressedAmbiguous,
			"wait_done", st.waitDone, "wait_timeout", st.waitTimeout)
		return
	}
	a.mu.Unlock()
}

// --- production client (adds the auto surface to *natsRefresher) ---

// Status implements autoRefresher: the instance KV status of the repository.
func (r *natsRefresher) Status(repo string) (refreshStatus, bool) {
	kv, err := r.kv()
	if err != nil {
		return refreshStatus{}, false
	}
	return readKVStatus(kv, refreshSlug(repo))
}

// Wait implements autoRefresher: poll for a terminal status (correlation by
// request id when want is set).
func (r *natsRefresher) Wait(ctx context.Context, repo, want string, wait time.Duration) bool {
	kv, err := r.kv()
	if err != nil {
		return false
	}
	out, err := r.poll(ctx, kv, refreshSlug(repo), wait, want)
	if err != nil {
		return false
	}
	return out.Status.State == "done" || out.Status.State == "failed"
}

// Trigger implements autoRefresher: publish an auto task to the instance
// subject (JetStream dedup by request id, as in Refresh).
func (r *natsRefresher) Trigger(repo string) (string, error) {
	js, err := r.nc.JetStream()
	if err != nil {
		return "", err
	}
	task := refreshTask{Repo: repo, RequestID: newRequestID(), TS: time.Now().Unix(), Origin: "auto"}
	data, err := json.Marshal(task)
	if err != nil {
		return "", err
	}
	msg := nats.NewMsg(r.names.Subject)
	msg.Header.Set("Nats-Msg-Id", task.RequestID)
	msg.Data = data
	if _, err := js.PublishMsg(msg); err != nil {
		return "", err
	}
	return task.RequestID, nil
}

// kv returns the instance status KV bucket.
func (r *natsRefresher) kv() (nats.KeyValue, error) {
	js, err := r.nc.JetStream()
	if err != nil {
		return nil, err
	}
	return js.KeyValue(r.names.KVBucket)
}
