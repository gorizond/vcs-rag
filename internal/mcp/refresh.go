package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nats-io/nats.go"

	"github.com/gorizond/vcs-rag/internal/config"
)

// Client-side refresh timings (T113); the topology names are built per
// instance by config.RefreshNames (T117/T119).
const (
	refreshPollInterval   = 500 * time.Millisecond
	refreshDefaultTimeout = 20 * time.Second
	refreshMaxTimeout     = 120 * time.Second
	refreshDoneFresh      = 30 * time.Second // a newer "done" is served from cache
	refreshLiveWindow     = 15 * time.Minute // a younger "in_progress" is polled, not republished
)

// refreshStatus is the read model of the refresh_status KV value
// (worker/refresh.go is the write side).
type refreshStatus struct {
	State          string    `json:"state"` // in_progress | done | failed
	RequestID      string    `json:"request_id,omitempty"`
	StartedAt      time.Time `json:"started_at,omitempty"`
	FinishedAt     time.Time `json:"finished_at,omitempty"`
	CommitsIndexed int       `json:"commits_indexed"`
	MetaUpdated    int       `json:"meta_updated"`
	Errors         []string  `json:"errors,omitempty"`
	DurationMS     int64     `json:"duration_ms"`
}

// refreshTask is the published request (worker/refresh.go reads it).
type refreshTask struct {
	Repo      string `json:"repo"`
	RequestID string `json:"request_id"`
	TS        int64  `json:"ts"`
}

// refreshOutcome is the client result: the last known status plus the flow flags.
type refreshOutcome struct {
	Status refreshStatus // done/failed, or in_progress after the poll timeout
	Cached bool          // served from a fresh KV done (no publication)
	Seen   bool          // the worker wrote a status for this key (worker_seen)
}

// refresher is the MCP-side refresh client (fake in tests).
type refresher interface {
	Refresh(ctx context.Context, repo string, timeout time.Duration) (refreshOutcome, error)
}

// --- tool input/output (T113) ---

type refreshRepoInput struct {
	Repo      string `json:"repo" jsonschema:"repository to refresh (namespace/path, or a unique suffix)"`
	TimeoutMS int    `json:"timeout_ms,omitempty" jsonschema:"how long to wait for completion (default 20000, maximum 120000)"`
}

type refreshDoneResponse struct {
	Status         string   `json:"status"` // done
	Repo           string   `json:"repo"`
	CommitsIndexed int      `json:"commits_indexed"`
	MetaUpdated    int      `json:"meta_updated"`
	Errors         []string `json:"errors"`
	DurationMS     int64    `json:"duration_ms"`
	Cached         bool     `json:"cached"`
}

type refreshInProgressResponse struct {
	Status     string `json:"status"` // in_progress
	Repo       string `json:"repo"`
	StartedAt  string `json:"started_at,omitempty"`
	ElapsedMS  int64  `json:"elapsed_ms"`
	WorkerSeen bool   `json:"worker_seen"`
}

// refreshRepo — on-demand repository refresh (T113): a fresh KV result is
// cached; a live run is polled; otherwise a task is published and polled.
// Concurrent calls for the same repository share one in-flight run.
func (t *tools) refreshRepo(ctx context.Context, _ *mcp.CallToolRequest, in refreshRepoInput) (*mcp.CallToolResult, any, error) {
	repo := strings.TrimSpace(in.Repo)
	if repo == "" {
		return nil, toolError{Code: codeRefreshUnavailable,
			Message: "refresh: repo is required — continue searching the current index"}, nil
	}
	if t.refresher == nil || t.inflight == nil {
		return nil, toolError{Code: codeRefreshUnavailable,
			Message: "refresh is not configured — continue searching the current index"}, nil
	}

	start := time.Now()
	out, err := t.inflight.do(refreshSlug(repo), func() (refreshOutcome, error) {
		return t.refresher.Refresh(ctx, repo, clampRefreshTimeout(in.TimeoutMS))
	})
	if err != nil {
		te := errorResult(ctx, codeRefreshUnavailable, err)
		te.Message += " — continue searching the current index"
		return nil, te, nil
	}

	switch out.Status.State {
	case "done":
		return nil, refreshDoneResponse{
			Status:         "done",
			Repo:           repo,
			CommitsIndexed: out.Status.CommitsIndexed,
			MetaUpdated:    out.Status.MetaUpdated,
			Errors:         nonNilStrings(out.Status.Errors),
			DurationMS:     out.Status.DurationMS,
			Cached:         out.Cached,
		}, nil
	case "failed":
		msg := "refresh failed"
		if len(out.Status.Errors) > 0 {
			msg += ": " + strings.Join(out.Status.Errors, "; ")
		}
		return nil, toolError{Code: codeRefreshUnavailable,
			Message: msg + " — continue searching the current index"}, nil
	default: // in_progress: the task continues in background
		resp := refreshInProgressResponse{
			Status:     "in_progress",
			Repo:       repo,
			ElapsedMS:  time.Since(start).Milliseconds(),
			WorkerSeen: out.Seen,
		}
		if !out.Status.StartedAt.IsZero() {
			resp.StartedAt = out.Status.StartedAt.UTC().Format(time.RFC3339)
		}
		return nil, resp, nil
	}
}

// clampRefreshTimeout applies the default of 20 s and the cap of 120 s.
func clampRefreshTimeout(ms int) time.Duration {
	switch {
	case ms <= 0:
		return refreshDefaultTimeout
	case time.Duration(ms)*time.Millisecond > refreshMaxTimeout:
		return refreshMaxTimeout
	default:
		return time.Duration(ms) * time.Millisecond
	}
}

// refreshSlug normalizes the request repo into the KV key form (the worker
// repeats this one-liner in repolock.go).
func refreshSlug(repo string) string {
	return strings.ReplaceAll(strings.Trim(strings.TrimSpace(repo), "/"), "/", "_")
}

// nonNilStrings keeps the JSON "errors" field an array (never null).
func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// --- in-flight share ---

// flightGroup shares one in-flight refresh between concurrent callers of the
// same repository: a single publication and a single poll (T113).
type flightGroup struct {
	mu    sync.Mutex
	calls map[string]*flightCall
}

type flightCall struct {
	done chan struct{}
	out  refreshOutcome
	err  error
}

func newFlightGroup() *flightGroup { return &flightGroup{calls: make(map[string]*flightCall)} }

// do runs fn once per key while it is in flight; duplicates wait for the result.
func (g *flightGroup) do(key string, fn func() (refreshOutcome, error)) (refreshOutcome, error) {
	g.mu.Lock()
	if c, ok := g.calls[key]; ok {
		g.mu.Unlock()
		<-c.done
		return c.out, c.err
	}
	c := &flightCall{done: make(chan struct{})}
	g.calls[key] = c
	g.mu.Unlock()

	c.out, c.err = fn()
	close(c.done)
	g.mu.Lock()
	delete(g.calls, key)
	g.mu.Unlock()
	return c.out, c.err
}

// --- production client ---

// natsRefresher — the JetStream-backed refresher (T113/T119). A temporarily
// unreachable NATS does not prevent the search server from starting: the
// connection retries in the background and per-request calls fail with a
// structured refresh_unavailable.
type natsRefresher struct {
	nc    *nats.Conn
	names config.RefreshTopology
}

// connectRefreshClient dials NATS for the MCP side (reconnect is perpetual);
// the subject and the KV bucket come from the instance names (T119).
func connectRefreshClient(url string, names config.RefreshTopology) (*natsRefresher, error) {
	nc, err := nats.Connect(url,
		nats.Name(names.ConnName+"-mcp"),
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("nats: connect: %w", err)
	}
	return &natsRefresher{nc: nc, names: names}, nil
}

// Close closes the connection.
func (r *natsRefresher) Close() { r.nc.Close() }

// Connected reports the current connection state (startup log).
func (r *natsRefresher) Connected() bool { return r.nc.Status() == nats.CONNECTED }

// Refresh implements refresher (T113).
func (r *natsRefresher) Refresh(ctx context.Context, repo string, timeout time.Duration) (refreshOutcome, error) {
	js, err := r.nc.JetStream()
	if err != nil {
		return refreshOutcome{}, fmt.Errorf("nats: %w", err)
	}
	kv, err := js.KeyValue(r.names.KVBucket)
	if err != nil {
		return refreshOutcome{}, fmt.Errorf("nats: KV %s: %w", r.names.KVBucket, err)
	}

	key := refreshSlug(repo)
	// cached / live: a fresh "done" is served directly; a younger "in_progress"
	// is polled without republishing (the worker owns the run).
	if st, ok := readKVStatus(kv, key); ok {
		switch {
		case st.State == "done" && time.Since(st.FinishedAt) < refreshDoneFresh:
			return refreshOutcome{Status: st, Cached: true, Seen: true}, nil
		case st.State == "in_progress" && time.Since(st.StartedAt) < refreshLiveWindow:
			return r.poll(ctx, kv, key, timeout)
		}
	}

	task := refreshTask{Repo: repo, RequestID: newRequestID(), TS: time.Now().Unix()}
	data, err := json.Marshal(task)
	if err != nil {
		return refreshOutcome{}, err
	}
	msg := nats.NewMsg(r.names.Subject)
	msg.Header.Set("Nats-Msg-Id", task.RequestID) // JetStream dedup window
	msg.Data = data
	if _, err := js.PublishMsg(msg); err != nil {
		return refreshOutcome{}, fmt.Errorf("nats: publish: %w", err)
	}
	return r.poll(ctx, kv, key, timeout)
}

// poll reads the KV status every 500 ms until done/failed or the timeout.
func (r *natsRefresher) poll(ctx context.Context, kv nats.KeyValue, key string, timeout time.Duration) (refreshOutcome, error) {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(refreshPollInterval)
	defer ticker.Stop()
	seen := false
	for {
		if st, ok := readKVStatus(kv, key); ok {
			switch st.State {
			case "done", "failed":
				return refreshOutcome{Status: st, Seen: true}, nil
			case "in_progress":
				seen = true
			}
		}
		if !time.Now().Before(deadline) {
			return refreshOutcome{Status: refreshStatus{State: "in_progress"}, Seen: seen}, nil
		}
		select {
		case <-ctx.Done():
			return refreshOutcome{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

// readKVStatus reads the status; a missing key (or an unreadable value) is "no status".
func readKVStatus(kv nats.KeyValue, key string) (refreshStatus, bool) {
	entry, err := kv.Get(key)
	if err != nil {
		return refreshStatus{}, false
	}
	var st refreshStatus
	if err := json.Unmarshal(entry.Value(), &st); err != nil {
		return refreshStatus{}, false
	}
	return st, true
}

// newRequestID returns a random 128-bit hex request id.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("req-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
