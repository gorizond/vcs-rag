package worker

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorizond/vcs-rag/internal/gitengine"
	"github.com/gorizond/vcs-rag/internal/provider"
	"github.com/gorizond/vcs-rag/internal/provider/fake"
)

// T111: exact match, unique suffix, ambiguity with a candidate list, not found.
func TestResolveRepo(t *testing.T) {
	repos := []provider.Repo{
		{ID: "1", Namespace: "e2e/grp", Path: "fixture"},
		{ID: "2", Namespace: "other", Path: "fixture"},
		{ID: "3", Namespace: "x", Path: "api"},
	}

	if r, err := resolveRepo(repos, "e2e/grp/fixture"); err != nil || r.ID != "1" {
		t.Errorf("exact: %+v, %v", r, err)
	}
	if r, err := resolveRepo(repos, "grp/fixture"); err != nil || r.ID != "1" {
		t.Errorf("unique suffix: %+v, %v", r, err)
	}
	if r, err := resolveRepo(repos, "x/api"); err != nil || r.ID != "3" {
		t.Errorf("suffix x/api: %+v, %v", r, err)
	}

	_, err := resolveRepo(repos, "fixture")
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous = %v, want an ambiguity error", err)
	}
	for _, name := range []string{"e2e/grp/fixture", "other/fixture"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("ambiguity error %q does not list %q", err, name)
		}
	}
	if _, err := resolveRepo(repos, "nope"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("not found = %v", err)
	}
	if _, err := resolveRepo(repos, "  "); err == nil {
		t.Error("empty name must be an error")
	}
}

// T111: a refresh indexes N new commits (repeat → 0) and updates the metadata;
// per-phase errors are collected without cancelling the next phase.
func TestRefreshOnceIndexesAndMeta(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not found: %v", err)
	}
	ctx := context.Background()
	src, shas := indexFixture(t, 3)
	repo := provider.Repo{ID: "1", Namespace: "grp", Path: "fixture", CloneURL: "file://" + src, DefaultBranch: "main"}
	deps, _ := newIndexDeps(t, repo)
	deps.VCS = &fake.Provider{
		Issues: map[string][]provider.Issue{
			"1": {{ID: 7, Title: "Issue", UpdatedAt: time.Unix(1000, 0)}},
		},
	}
	idx := &faultStore{}
	meta := &countingStore{}

	res := refreshOnce(ctx, deps, idx, meta, repo)
	if len(res.Errors) != 0 {
		t.Fatalf("first refresh errors = %v, want none", res.Errors)
	}
	if res.CommitsIndexed != 3 || res.MetaUpdated != 1 {
		t.Errorf("first refresh = %+v, want 3 commits / 1 meta", res)
	}

	res2 := refreshOnce(ctx, deps, idx, meta, repo)
	if len(res2.Errors) != 0 || res2.CommitsIndexed != 0 || res2.MetaUpdated != 0 {
		t.Errorf("repeat refresh = %+v, want 0/0 and no errors", res2)
	}

	statePath := gitengine.StatePath(deps.Engine.MirrorPath(deps.VCSType, repo))
	saved, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := saved.Branches["main"].IndexedCommits; len(got) != len(shas) {
		t.Errorf("state commits = %v, want %d", got, len(shas))
	}
}

// T111: an index-phase failure yields a partial result (checkpoints kept) and
// does not cancel the metadata phase.
func TestRefreshOncePartialPhase(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not found: %v", err)
	}
	ctx := context.Background()
	src, _ := indexFixture(t, 3)
	repo := provider.Repo{ID: "1", Namespace: "grp", Path: "fixture", CloneURL: "file://" + src, DefaultBranch: "main"}
	deps, _ := newIndexDeps(t, repo)
	deps.VCS = &fake.Provider{
		Issues: map[string][]provider.Issue{
			"1": {{ID: 7, Title: "Issue", UpdatedAt: time.Unix(1000, 0)}},
		},
	}
	statePath := gitengine.StatePath(deps.Engine.MirrorPath(deps.VCSType, repo))

	// fault injection: the 3rd commit upsert fails → K−1 checkpoints kept.
	idx := &faultStore{failAt: 3}
	meta := &countingStore{}
	res := refreshOnce(ctx, deps, idx, meta, repo)
	if len(res.Errors) == 0 || !strings.Contains(strings.Join(res.Errors, "; "), "index:") {
		t.Fatalf("errors = %v, want an index phase error", res.Errors)
	}
	if res.CommitsIndexed != 2 {
		t.Errorf("partial commits = %d, want 2 (checkpointed)", res.CommitsIndexed)
	}
	if res.MetaUpdated != 1 {
		t.Errorf("meta after the partial index = %d, want 1 (best-effort phase)", res.MetaUpdated)
	}
	saved, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := saved.Branches["main"].IndexedCommits; len(got) != 2 {
		t.Fatalf("checkpointed commits = %v, want 2", got)
	}

	// the retry finishes the remainder only.
	res2 := refreshOnce(ctx, deps, idx, meta, repo)
	if len(res2.Errors) != 0 || res2.CommitsIndexed != 1 {
		t.Errorf("retry = %+v, want 1 commit and no errors", res2)
	}
	saved, err = LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := saved.Branches["main"].IndexedCommits; len(got) != 3 {
		t.Errorf("final commits = %v, want 3", got)
	}
}

// T111: a mirror-phase failure does not cancel index/metadata (best-effort).
func TestRefreshOnceMirrorErrorBestEffort(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not found: %v", err)
	}
	ctx := context.Background()
	src, _ := indexFixture(t, 2)
	repo := provider.Repo{ID: "1", Namespace: "grp", Path: "fixture", CloneURL: "file://" + src, DefaultBranch: "main"}
	deps, _ := newIndexDeps(t, repo)
	deps.VCS = &fake.Provider{
		Issues: map[string][]provider.Issue{
			"1": {{ID: 7, Title: "Issue", UpdatedAt: time.Unix(1000, 0)}},
		},
	}

	// the initial clone succeeds; then the remote becomes unusable.
	mirror, err := deps.Engine.EnsureMirror(ctx, deps.VCSType, repo)
	if err != nil {
		t.Fatalf("EnsureMirror: %v", err)
	}
	bogus := "file://" + filepath.Join(t.TempDir(), "void.git")
	if out, err := exec.Command("git", "-C", mirror, "remote", "set-url", "origin", bogus).CombinedOutput(); err != nil {
		t.Fatalf("remote set-url: %v: %s", err, out)
	}

	res := refreshOnce(ctx, deps, &faultStore{}, &countingStore{}, repo)
	if len(res.Errors) != 1 || !strings.Contains(res.Errors[0], "mirror:") {
		t.Fatalf("errors = %v, want exactly the mirror phase error", res.Errors)
	}
	if res.CommitsIndexed != 2 || res.MetaUpdated != 1 {
		t.Errorf("best-effort refresh = %+v, want 2 commits / 1 meta", res)
	}
}
