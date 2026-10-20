package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/gorizond/vcs-rag/internal/config"
	"github.com/gorizond/vcs-rag/internal/provider"
	"github.com/gorizond/vcs-rag/internal/provider/fake"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestListOnceStoresSnapshot: a successful pass stores repositories in the snapshot.
func TestListOnceStoresSnapshot(t *testing.T) {
	snap := &snapshot{}
	repos := []provider.Repo{{ID: "1", Namespace: "g", Path: "g/a", CloneURL: "https://example.com/g/a.git"}}
	listOnce(context.Background(), &fake.Provider{Repos: repos}, &config.Config{}, testLogger(), snap)
	if got := snap.get(); len(got) != 1 || got[0].ID != "1" {
		t.Fatalf("snapshot = %+v", got)
	}
}

// TestListOnceKeepsSnapshotOnError: a listing error does not wipe the snapshot (FR-014).
func TestListOnceKeepsSnapshotOnError(t *testing.T) {
	snap := &snapshot{}
	p := &fake.Provider{Repos: []provider.Repo{{ID: "1"}}}
	listOnce(context.Background(), p, &config.Config{}, testLogger(), snap)

	p.Repos = nil
	p.ErrListRepositories = errors.New("boom")
	listOnce(context.Background(), p, &config.Config{}, testLogger(), snap)
	if got := snap.get(); len(got) != 1 {
		t.Fatalf("snapshot wiped on error: %+v", got)
	}
}

// countingProvider wraps the fake provider and counts ListRepositories calls.
type countingProvider struct {
	fake.Provider
	calls int
}

func (c *countingProvider) ListRepositories(ctx context.Context) ([]provider.Repo, error) {
	c.calls++
	return c.Provider.ListRepositories(ctx)
}

// T143: within the cache window the snapshot is reused without an API call;
// after the window it refreshes; a zero window always refreshes (negative
// control — the previous behavior for every existing instance).
func TestDiscoveryCacheWindow(t *testing.T) {
	ctx := context.Background()
	p := &countingProvider{}
	p.Repos = []provider.Repo{{ID: "1", Namespace: "g", Path: "a"}}
	snap := &snapshot{}
	var buf syncBuffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// no snapshot yet → the first call always fetches.
	listOnceCached(ctx, p, &config.Config{Worker: config.Worker{DiscoveryMinInterval: time.Hour}}, logger, snap)
	if p.calls != 1 || len(snap.get()) != 1 {
		t.Fatalf("first call: calls=%d snapshot=%d, want to fetch", p.calls, len(snap.get()))
	}

	// within the window: no API call, the snapshot is reused.
	before := snap.at()
	listOnceCached(ctx, p, &config.Config{Worker: config.Worker{DiscoveryMinInterval: time.Hour}}, logger, snap)
	if p.calls != 1 {
		t.Fatalf("cached call fetched again: calls=%d, want 1", p.calls)
	}
	if !snap.at().Equal(before) {
		t.Error("the cached call touched the snapshot timestamp")
	}
	if !strings.Contains(buf.String(), "discovery: cached") {
		t.Errorf("the cache debug line is missing: %q", buf.String())
	}

	// a stale snapshot (older than the window): refresh as usual.
	setSnap(snap, snap.get(), time.Now().Add(-2*time.Hour))
	listOnceCached(ctx, p, &config.Config{Worker: config.Worker{DiscoveryMinInterval: time.Hour}}, logger, snap)
	if p.calls != 2 {
		t.Fatalf("stale window: calls=%d, want a refresh", p.calls)
	}

	// the zero window keeps the previous behavior: every call refreshes.
	p.calls = 0
	zero := &snapshot{}
	zeroCfg := &config.Config{}
	listOnceCached(ctx, p, zeroCfg, logger, zero)
	listOnceCached(ctx, p, zeroCfg, logger, zero)
	if p.calls != 2 {
		t.Errorf("zero window: calls=%d, want 2 (no cache)", p.calls)
	}
}

// discoveryCfg builds a discovery config with an explicit retry policy — the
// tiny floor/cap keep the tests fast (production defaults 60s/15m).
func discoveryCfg(floor, cap time.Duration, retries int) *config.Config {
	return &config.Config{Worker: config.Worker{
		RetryFloor: floor, RetryCap: cap, MaxRetries: retries,
	}}
}

// rateLimitListingErr is the observed discovery failure shape (the classifier
// also recognizes the typed GitHub *RateLimitError).
func rateLimitListingErr() error {
	return errors.New("GET https://api.github.com/orgs/gorizond/repos: 403 API rate limit exceeded (429)")
}

// flakyListingProvider serves the fake's repos but fails the first failCalls
// listings with ErrListRepositories, then clears the error (recovery).
type flakyListingProvider struct {
	fake.Provider
	failCalls int
	calls     int
}

func (p *flakyListingProvider) ListRepositories(ctx context.Context) ([]provider.Repo, error) {
	p.calls++
	if p.calls <= p.failCalls {
		return nil, p.ErrListRepositories
	}
	p.ErrListRepositories = nil
	return p.Provider.ListRepositories(ctx)
}

// T061 (FR-001/FR-007): a transient rate limit pauses and retries; the second
// attempt succeeds, the snapshot is populated and no ERROR is logged.
func TestListOnceRetriesRateLimit(t *testing.T) {
	var buf syncBuffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	snap := &snapshot{}
	p := &flakyListingProvider{failCalls: 1}
	p.Repos = []provider.Repo{{ID: "1", Namespace: "g", Path: "a"}}
	p.ErrListRepositories = rateLimitListingErr()

	listOnce(context.Background(), p, discoveryCfg(time.Millisecond, time.Millisecond, 3), logger, snap)

	if p.calls != 2 {
		t.Fatalf("ListRepositories calls = %d, want 2 (one retry)", p.calls)
	}
	if got := snap.get(); len(got) != 1 || got[0].ID != "1" {
		t.Fatalf("snapshot = %+v, want the recovered listing", got)
	}
	log := buf.String()
	if strings.Contains(log, "level=ERROR") {
		t.Errorf("a transient rate limit logged ERROR: %s", log)
	}
	if !strings.Contains(log, "level=WARN") || !strings.Contains(log, "retry_after=") ||
		!strings.Contains(log, "attempt=1") {
		t.Errorf("paused attempt is not logged with retry_after/attempt: %s", log)
	}
}

// T061: persistent rate limiting retries exactly MaxRetries times, then logs a
// single ERROR and keeps the previous snapshot.
func TestListOnceRateLimitRetriesExhausted(t *testing.T) {
	var buf syncBuffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	snap := &snapshot{}
	snap.set([]provider.Repo{{ID: "old", Namespace: "g", Path: "old"}})
	p := &flakyListingProvider{failCalls: 100}
	p.ErrListRepositories = rateLimitListingErr()

	listOnce(context.Background(), p, discoveryCfg(time.Millisecond, time.Millisecond, 2), logger, snap)

	if p.calls != 3 {
		t.Fatalf("ListRepositories calls = %d, want 3 (initial + MaxRetries=2)", p.calls)
	}
	if got := snap.get(); len(got) != 1 || got[0].ID != "old" {
		t.Fatalf("snapshot changed after exhausted retries: %+v", got)
	}
	if n := strings.Count(buf.String(), "level=ERROR"); n != 1 {
		t.Errorf("ERROR lines = %d, want exactly 1: %s", n, buf.String())
	}
}

// T061: a non-rate-limit error keeps the previous behaviour — one attempt, one ERROR.
func TestListOnceNonRateLimitNoRetry(t *testing.T) {
	var buf syncBuffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	snap := &snapshot{}
	p := &flakyListingProvider{failCalls: 100}
	p.ErrListRepositories = errors.New("boom")

	listOnce(context.Background(), p, discoveryCfg(time.Millisecond, time.Millisecond, 5), logger, snap)

	if p.calls != 1 {
		t.Fatalf("ListRepositories calls = %d, want 1 (no retry)", p.calls)
	}
	if len(snap.get()) != 0 {
		t.Fatalf("snapshot populated on failure: %+v", snap.get())
	}
	if n := strings.Count(buf.String(), "level=ERROR"); n != 1 {
		t.Errorf("ERROR lines = %d, want 1: %s", n, buf.String())
	}
}

// T061: cancelling the context during the pause returns quietly (normal
// shutdown), with no ERROR and the snapshot untouched.
func TestListOnceRateLimitCancelDuringPause(t *testing.T) {
	var buf syncBuffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	snap := &snapshot{}
	p := &flakyListingProvider{failCalls: 100}
	p.ErrListRepositories = rateLimitListingErr()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	listOnce(ctx, p, discoveryCfg(200*time.Millisecond, 200*time.Millisecond, 3), logger, snap)
	cancel()

	if p.calls != 1 {
		t.Errorf("ListRepositories calls = %d, want 1 (cancelled in the pause)", p.calls)
	}
	if len(snap.get()) != 0 {
		t.Errorf("snapshot populated after cancellation: %+v", snap.get())
	}
	if strings.Contains(buf.String(), "level=ERROR") {
		t.Errorf("cancellation logged ERROR: %s", buf.String())
	}
}
