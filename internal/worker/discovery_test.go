package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

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
