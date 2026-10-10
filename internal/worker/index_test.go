package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gorizond/vcs-rag/internal/config"
	"github.com/gorizond/vcs-rag/internal/gitengine"
	"github.com/gorizond/vcs-rag/internal/provider"
	"github.com/gorizond/vcs-rag/internal/store"
)

// runGitT runs git in dir (local fixture helper).
func runGitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// indexFixture creates a source repository with n commits (each adds a unique
// text file) and returns its path plus the main shas oldest→newest.
func indexFixture(t *testing.T, n int) (string, []string) {
	t.Helper()
	dir := t.TempDir()
	runGitT(t, dir, "init", "-b", "main")
	for i := 1; i <= n; i++ {
		name := fmt.Sprintf("file%d.txt", i)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(fmt.Sprintf("content of file %d\n", i)), 0o644); err != nil {
			t.Fatal(err)
		}
		runGitT(t, dir, "add", ".")
		runGitT(t, dir, "commit", "-m", fmt.Sprintf("c%d", i))
	}
	return dir, strings.Fields(runGitT(t, dir, "rev-list", "--reverse", "main"))
}

// newIndexDeps builds RepoDeps for an index pass without Qdrant (the store is
// passed to processRepoOnce separately).
func newIndexDeps(t *testing.T, repo provider.Repo) (RepoDeps, *countingEmbed) {
	t.Helper()
	cfg := &config.Config{}
	cfg.Worker = config.Worker{HistoryMaxCommits: 100, MaxFileBytes: 1 << 20}
	emb := &countingEmbed{}
	return RepoDeps{
		VCSType: "gitlab",
		Engine:  gitengine.New(t.TempDir()),
		Embed:   emb,
		Cfg:     cfg,
		Logger:  testLogger(),
	}, emb
}

// faultStore is an indexStore double: counts successful points, fails at the
// K-th upsert (T088 fault injection) and fires a one-shot hook on the first
// operation (T089: simulating a concurrent lane writing to the sidecar).
type faultStore struct {
	upserts int
	points  int
	failAt  int // fail exactly the failAt-th upsert (0 — never)
	firstOp func()
}

func (f *faultStore) tick() {
	if f.firstOp != nil {
		fn := f.firstOp
		f.firstOp = nil
		fn()
	}
}

func (f *faultStore) DeleteCodeFile(context.Context, string, string, string) error {
	f.tick()
	return nil
}

func (f *faultStore) DeleteBranch(context.Context, string, string) error {
	f.tick()
	return nil
}

func (f *faultStore) DeleteCodeCommits(context.Context, string, string, []string) error {
	f.tick()
	return nil
}

func (f *faultStore) Upsert(_ context.Context, points []store.Point) error {
	f.tick()
	f.upserts++
	if f.upserts == f.failAt { // one-shot: the K-th upsert fails, the retry succeeds
		return errors.New("injected upsert failure")
	}
	f.points += len(points)
	return nil
}

// TestIndexCheckpointResumes — T088: a store failure at the K-th commit keeps
// exactly K−1 checkpointed commits; the retry indexes only the remainder and
// never touches the meta section.
func TestIndexCheckpointResumes(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not found: %v", err)
	}
	ctx := context.Background()
	const k = 3
	src, shas := indexFixture(t, k) // shas: oldest→newest
	repo := provider.Repo{ID: "1", Namespace: "grp", Path: "fixture", CloneURL: "file://" + src, DefaultBranch: "main"}
	deps, emb := newIndexDeps(t, repo)
	statePath := gitengine.StatePath(deps.Engine.MirrorPath(deps.VCSType, repo))

	// the meta section belongs to the metadata lane — index writes must not touch it.
	if err := updateState(statePath, func(st *MirrorState) { st.Meta["issue:1"] = 42 }); err != nil {
		t.Fatal(err)
	}

	// 1) fault injection: the upsert of the K-th commit fails.
	st := &faultStore{failAt: k}
	if err := processRepoOnce(ctx, deps, repo, st); err == nil {
		t.Fatal("expected the injected failure")
	}
	saved, err := LoadState(statePath)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	checkpoint := saved.Branches["main"]
	if len(checkpoint.IndexedCommits) != k-1 {
		t.Fatalf("checkpointed commits = %v, want %d", checkpoint.IndexedCommits, k-1)
	}
	want := []string{shas[k-2], shas[k-3]} // processed commits in window order (newest→oldest)
	if !reflect.DeepEqual(checkpoint.IndexedCommits, want) {
		t.Errorf("checkpointed commits = %v, want %v", checkpoint.IndexedCommits, want)
	}
	if saved.Meta["issue:1"] != 42 {
		t.Errorf("meta clobbered by the index checkpoint: %v", saved.Meta)
	}

	// 2) retry: only the remainder is embedded/indexed.
	before := emb.calls
	pointsBefore := st.points
	if err := processRepoOnce(ctx, deps, repo, st); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if emb.calls != before+1 {
		t.Errorf("retry embeddings = %d, want 1 (the remainder)", emb.calls-before)
	}
	if st.points <= pointsBefore {
		t.Errorf("retry wrote no points (points %d→%d)", pointsBefore, st.points)
	}
	saved, err = LoadState(statePath)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got := saved.Branches["main"].IndexedCommits; !reflect.DeepEqual(got, []string{shas[2], shas[1], shas[0]}) {
		t.Errorf("final commits = %v, want the full window", got)
	}
	if saved.Meta["issue:1"] != 42 {
		t.Errorf("meta clobbered by the index pass: %v", saved.Meta)
	}
}

// TestIndexPassPreservesConcurrentLanes — T089: a meta/last_fetch_at write
// landing DURING the index pass (simulated in the store hook, after the state
// was loaded) survives the pass, and a following no-change pass keeps it.
func TestIndexPassPreservesConcurrentLanes(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not found: %v", err)
	}
	ctx := context.Background()
	src, _ := indexFixture(t, 2)
	repo := provider.Repo{ID: "1", Namespace: "grp", Path: "fixture", CloneURL: "file://" + src, DefaultBranch: "main"}
	deps, _ := newIndexDeps(t, repo)
	statePath := gitengine.StatePath(deps.Engine.MirrorPath(deps.VCSType, repo))

	// a new commit arrives in the source; the mirror lane fetches it (T083).
	if err := os.WriteFile(filepath.Join(src, "extra.txt"), []byte("extra\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, src, "add", ".")
	runGitT(t, src, "commit", "-m", "c3")
	if _, err := deps.Engine.EnsureMirror(ctx, "gitlab", repo); err != nil {
		t.Fatalf("mirror lane fetch: %v", err)
	}

	// the other lanes write their sections mid-pass: after the index lane has
	// loaded the state, before its terminal write.
	fetchAt := time.Now().UTC().Truncate(time.Second)
	st := &faultStore{}
	st.firstOp = func() {
		if err := updateState(statePath, func(s *MirrorState) {
			s.Meta["issue:1"] = 42
			s.LastFetchAt = fetchAt
		}); err != nil {
			t.Errorf("concurrent lane write: %v", err)
		}
	}
	if err := processRepoOnce(ctx, deps, repo, st); err != nil {
		t.Fatalf("index pass: %v", err)
	}
	saved, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Meta["issue:1"] != 42 {
		t.Errorf("meta clobbered by the index pass: %v", saved.Meta)
	}
	if !saved.LastFetchAt.Equal(fetchAt) {
		t.Errorf("last_fetch_at clobbered: %v, want %v", saved.LastFetchAt, fetchAt)
	}

	// a no-change pass still preserves both sections.
	if err := processRepoOnce(ctx, deps, repo, st); err != nil {
		t.Fatalf("no-change pass: %v", err)
	}
	saved, err = LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Meta["issue:1"] != 42 || !saved.LastFetchAt.Equal(fetchAt) {
		t.Errorf("no-change pass clobbered the other lanes: meta=%v last_fetch_at=%v",
			saved.Meta, saved.LastFetchAt)
	}
}
