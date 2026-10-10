package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/gorizond/vcs-rag/internal/provider"
	"github.com/gorizond/vcs-rag/internal/provider/fake"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestListOnceStoresSnapshot: a successful pass stores repositories in the snapshot.
func TestListOnceStoresSnapshot(t *testing.T) {
	snap := &snapshot{}
	repos := []provider.Repo{{ID: "1", Namespace: "g", Path: "g/a", CloneURL: "https://example.com/g/a.git"}}
	listOnce(context.Background(), &fake.Provider{Repos: repos}, testLogger(), snap)
	if got := snap.get(); len(got) != 1 || got[0].ID != "1" {
		t.Fatalf("snapshot = %+v", got)
	}
}

// TestListOnceKeepsSnapshotOnError: a listing error does not wipe the snapshot (FR-014).
func TestListOnceKeepsSnapshotOnError(t *testing.T) {
	snap := &snapshot{}
	p := &fake.Provider{Repos: []provider.Repo{{ID: "1"}}}
	listOnce(context.Background(), p, testLogger(), snap)

	p.Repos = nil
	p.ErrListRepositories = errors.New("boom")
	listOnce(context.Background(), p, testLogger(), snap)
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
	listOnceCached(ctx, p, time.Hour, logger, snap)
	if p.calls != 1 || len(snap.get()) != 1 {
		t.Fatalf("first call: calls=%d snapshot=%d, want to fetch", p.calls, len(snap.get()))
	}

	// within the window: no API call, the snapshot is reused.
	before := snap.at()
	listOnceCached(ctx, p, time.Hour, logger, snap)
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
	listOnceCached(ctx, p, time.Hour, logger, snap)
	if p.calls != 2 {
		t.Fatalf("stale window: calls=%d, want a refresh", p.calls)
	}

	// the zero window keeps the previous behavior: every call refreshes.
	p.calls = 0
	zero := &snapshot{}
	listOnceCached(ctx, p, 0, logger, zero)
	listOnceCached(ctx, p, 0, logger, zero)
	if p.calls != 2 {
		t.Errorf("zero window: calls=%d, want 2 (no cache)", p.calls)
	}
}
