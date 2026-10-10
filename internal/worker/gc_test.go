package worker

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorizond/vcs-rag/internal/config"
	"github.com/gorizond/vcs-rag/internal/gitengine"
	"github.com/gorizond/vcs-rag/internal/provider"
)

// fakeGCStore counts the GC store calls and serves canned point counts.
type fakeGCStore struct {
	counts    []string
	deletes   []string
	points    map[string]uint64
	countErr  error
	deleteErr error
}

func (f *fakeGCStore) CountRepoPoints(_ context.Context, _, ns, repo string) (uint64, error) {
	key := gcKey(ns, repo)
	f.counts = append(f.counts, key)
	if f.countErr != nil {
		return 0, f.countErr
	}
	return f.points[key], nil
}

func (f *fakeGCStore) DeleteRepoPoints(_ context.Context, _, ns, repo string) error {
	f.deletes = append(f.deletes, gcKey(ns, repo))
	return f.deleteErr
}

// gcRepo builds a repository listing entry.
func gcRepo(ns, path string) provider.Repo {
	return provider.Repo{ID: ns + "/" + path, Namespace: ns, Path: path}
}

// setSnap stores a listing with an explicit timestamp (staleness control).
func setSnap(snap *snapshot, repos []provider.Repo, at time.Time) {
	snap.set(repos)
	snap.mu.Lock()
	snap.stamp = at
	snap.mu.Unlock()
}

// newGCTestRunner builds a runner over temp mirrors and fakes (dry-run off —
// the real mode is exercised per default; dry-run tests override the config).
func newGCTestRunner(t *testing.T, dir string, snap *snapshot, reg *gcRegistry, logger *slog.Logger) *gcRunner {
	t.Helper()
	cfg := &config.Config{Worker: config.Worker{
		MirrorsDir: dir,
		Interval:   time.Minute,
		GCInterval: 15 * time.Minute,
	}}
	deps := RepoDeps{VCSType: "gitlab", Engine: gitengine.New(dir), Logger: logger}
	return newGCRunner(cfg, deps, snap, reg)
}

// makeMirror creates the bare-mirror directory and its state sidecar.
func makeMirror(t *testing.T, dir, ns, path string) (mirror, state string) {
	t.Helper()
	mirror = filepath.Join(dir, "gitlab", ns, path+".git")
	if err := os.MkdirAll(mirror, 0o755); err != nil {
		t.Fatal(err)
	}
	state = gitengine.StatePath(mirror)
	if err := os.WriteFile(state, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state+".tmp", []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return mirror, state
}

// T140: a repository absent from two consecutive listings (and past the grace
// period) is deleted exactly once — points, mirror, state and registry entry.
func TestGCDisappearedTwoPassesThenDelete(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	dir := t.TempDir()
	mirror, state := makeMirror(t, dir, "grp", "fixture")

	reg := &gcRegistry{Entries: map[string]*gcEntry{
		gcKey("grp", "other"): {Namespace: "grp", Path: "other", LastSeenAt: now},
		gcKey("grp", "fixture"): {Namespace: "grp", Path: "fixture",
			LastSeenAt: now.Add(-30 * time.Minute), FirstMissingAt: now.Add(-11 * time.Minute), MissStreak: 1},
	}}
	snap := &snapshot{}
	setSnap(snap, []provider.Repo{gcRepo("grp", "other")}, now)
	r := newGCTestRunner(t, dir, snap, reg, testLogger())
	r.now = func() time.Time { return now }
	st := &fakeGCStore{points: map[string]uint64{gcKey("grp", "fixture"): 5}}

	sum := r.pass(ctx, st)
	if len(st.deletes) != 1 || st.deletes[0] != gcKey("grp", "fixture") {
		t.Fatalf("deletes = %v, want exactly grp/fixture", st.deletes)
	}
	if len(st.counts) != 1 {
		t.Errorf("counts = %v, want the delete read-back", st.counts)
	}
	if sum.gcDeletedRepos.Load() != 1 || sum.gcDeletedPoints.Load() != 5 {
		t.Errorf("counters = %d repos / %d points, want 1/5",
			sum.gcDeletedRepos.Load(), sum.gcDeletedPoints.Load())
	}
	if _, ok := reg.Entries[gcKey("grp", "fixture")]; ok {
		t.Error("the registry entry of the deleted repository was not removed")
	}
	if _, err := os.Stat(mirror); !os.IsNotExist(err) {
		t.Errorf("mirror %s still exists (err=%v)", mirror, err)
	}
	for _, p := range []string{state, state + ".tmp"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("state %s still exists (err=%v)", p, err)
		}
	}
	if _, ok := reg.Entries[gcKey("grp", "other")]; !ok {
		t.Error("the live repository entry disappeared")
	}

	// the second pass must not delete again (the entry is gone).
	now = now.Add(time.Minute)
	setSnap(snap, []provider.Repo{gcRepo("grp", "other")}, now)
	r.pass(ctx, st)
	if len(st.deletes) != 1 {
		t.Errorf("deletes after the second pass = %v, want no repeat", st.deletes)
	}
}

// T140: a returned repository resets the streak — no deletion on a later
// single disappearance.
func TestGCReturnedResetsStreak(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	dir := t.TempDir()
	reg := &gcRegistry{Entries: map[string]*gcEntry{
		gcKey("grp", "fixture"): {Namespace: "grp", Path: "fixture", LastSeenAt: now},
	}}
	snap := &snapshot{}
	setSnap(snap, []provider.Repo{gcRepo("grp", "other")}, now) // fixture missing
	r := newGCTestRunner(t, dir, snap, reg, testLogger())
	r.now = func() time.Time { return now }
	st := &fakeGCStore{}

	if sum := r.pass(ctx, st); sum.gcSuppressedGrace.Load() != 1 {
		t.Fatalf("first missing pass: grace = %d, want 1", sum.gcSuppressedGrace.Load())
	}
	if e := reg.Entries[gcKey("grp", "fixture")]; e.MissStreak != 1 {
		t.Fatalf("streak after one missing pass = %d, want 1", e.MissStreak)
	}

	// it returned: the streak resets.
	now = now.Add(time.Minute)
	setSnap(snap, []provider.Repo{gcRepo("grp", "fixture"), gcRepo("grp", "other")}, now)
	r.pass(ctx, st)
	if e := reg.Entries[gcKey("grp", "fixture")]; e.MissStreak != 0 || !e.FirstMissingAt.IsZero() {
		t.Fatalf("returned repo not reset: %+v", e)
	}

	// missing again well past the grace: only the first streak pass, no delete.
	now = now.Add(time.Hour)
	setSnap(snap, []provider.Repo{gcRepo("grp", "other")}, now)
	sum := r.pass(ctx, st)
	if len(st.deletes) != 0 || sum.gcSuppressedGrace.Load() != 1 {
		t.Errorf("deletes = %v, grace = %d; want none/1", st.deletes, sum.gcSuppressedGrace.Load())
	}
	if e := reg.Entries[gcKey("grp", "fixture")]; e.MissStreak != 1 {
		t.Errorf("streak after the second disappearance = %d, want 1", e.MissStreak)
	}
}

// T140: an empty snapshot freezes the streaks for three passes, then resumes.
func TestGCAnomalyFreezeEmptySnapshot(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	dir := t.TempDir()
	reg := &gcRegistry{Entries: map[string]*gcEntry{
		gcKey("grp", "fixture"): {Namespace: "grp", Path: "fixture", LastSeenAt: now,
			MissStreak: 1, FirstMissingAt: now.Add(-time.Minute)},
	}}
	snap := &snapshot{} // empty — the listing failed
	r := newGCTestRunner(t, dir, snap, reg, testLogger())
	r.now = func() time.Time { return now }
	st := &fakeGCStore{}

	for i := 1; i <= gcAnomalyFreezePasses; i++ {
		sum := r.pass(ctx, st)
		if sum.gcAnomalies.Load() != 1 {
			t.Fatalf("pass %d: anomalies = %d, want 1 (per pass)", i, sum.gcAnomalies.Load())
		}
		if e := reg.Entries[gcKey("grp", "fixture")]; e.MissStreak != 1 {
			t.Fatalf("pass %d moved the streak: %+v", i, e)
		}
	}
	// the fourth consecutive anomaly resumes the streak accounting.
	sum := r.pass(ctx, st)
	if sum.gcAnomalies.Load() != 1 {
		t.Errorf("anomalies = %d, want 1", sum.gcAnomalies.Load())
	}
	if e := reg.Entries[gcKey("grp", "fixture")]; e.MissStreak != 2 {
		t.Errorf("streak did not resume: %+v", e)
	}
	if len(st.deletes) != 0 {
		t.Errorf("deletes = %v, want none (freeze protects)", st.deletes)
	}
}

// T140: a >30% snapshot drop (with ≥10 known repos) freezes the GC.
func TestGCAnomalySnapshotDrop(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	dir := t.TempDir()
	var repos []provider.Repo
	for i := 0; i < 12; i++ {
		repos = append(repos, gcRepo("grp", string(rune('a'+i))))
	}
	reg := &gcRegistry{Entries: map[string]*gcEntry{}}
	snap := &snapshot{}
	setSnap(snap, repos, now)
	r := newGCTestRunner(t, dir, snap, reg, testLogger())
	r.now = func() time.Time { return now }
	st := &fakeGCStore{}

	if sum := r.pass(ctx, st); sum.gcAnomalies.Load() != 0 {
		t.Fatalf("the first pass is not anomalous: %d", sum.gcAnomalies.Load())
	}
	if r.prevKnown != 12 {
		t.Fatalf("prevKnown = %d, want 12", r.prevKnown)
	}

	dropped := repos[:6]
	for i := 0; i < gcAnomalyFreezePasses; i++ {
		now = now.Add(time.Minute)
		setSnap(snap, dropped, now)
		sum := r.pass(ctx, st)
		if sum.gcAnomalies.Load() == 0 {
			t.Fatalf("pass %d: drop was not classified as an anomaly", i)
		}
		for key, e := range reg.Entries {
			if e.MissStreak != 0 {
				t.Fatalf("pass %d moved the streak of %s: %+v", i, key, e)
			}
		}
	}
	// resume: the six missing repositories start their streaks.
	now = now.Add(time.Minute)
	setSnap(snap, dropped, now)
	sum := r.pass(ctx, st)
	if got := sum.gcVanished.Load(); got != 6 {
		t.Errorf("vanished = %d, want 6", got)
	}
	if len(st.deletes) != 0 {
		t.Errorf("deletes = %v, want none (grace)", st.deletes)
	}
}

// T140: a stale snapshot freezes the GC until a fresh listing arrives.
func TestGCStaleSnapshotFreezes(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	dir := t.TempDir()
	reg := &gcRegistry{Entries: map[string]*gcEntry{
		gcKey("grp", "fixture"): {Namespace: "grp", Path: "fixture", LastSeenAt: now},
	}}
	snap := &snapshot{}
	setSnap(snap, []provider.Repo{gcRepo("grp", "other")}, now.Add(-11*time.Minute))
	r := newGCTestRunner(t, dir, snap, reg, testLogger())
	r.now = func() time.Time { return now }
	st := &fakeGCStore{}

	sum := r.pass(ctx, st)
	if sum.gcAnomalies.Load() != 1 {
		t.Fatalf("stale snapshot not frozen: %d", sum.gcAnomalies.Load())
	}
	if e := reg.Entries[gcKey("grp", "fixture")]; e.MissStreak != 0 {
		t.Fatalf("stale pass moved the streak: %+v", e)
	}

	// a fresh listing resumes.
	setSnap(snap, []provider.Repo{gcRepo("grp", "other")}, now)
	sum = r.pass(ctx, st)
	if sum.gcVanished.Load() != 1 {
		t.Errorf("vanished = %d, want 1", sum.gcVanished.Load())
	}
	if e := reg.Entries[gcKey("grp", "fixture")]; e.MissStreak != 1 {
		t.Errorf("streak = %d, want 1", e.MissStreak)
	}
}

// T140: at most 10 deletions per pass, oldest disappearance first; the rest
// is counted as suppressed by the cap.
func TestGCCapAndOrder(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	dir := t.TempDir()
	reg := &gcRegistry{Entries: map[string]*gcEntry{}}
	var oldestFirst []string
	for i := 1; i <= 15; i++ {
		key := gcKey("grp", "r"+two(i))
		// all past the grace period; i=15 has the oldest first_missing_at.
		reg.Entries[key] = &gcEntry{Namespace: "grp", Path: "r" + two(i),
			LastSeenAt: now.Add(-time.Hour), FirstMissingAt: now.Add(-time.Duration(10+i) * time.Minute), MissStreak: 2}
		oldestFirst = append([]string{key}, oldestFirst...) // i=15 first (oldest)
	}
	snap := &snapshot{}
	setSnap(snap, []provider.Repo{gcRepo("grp", "alive")}, now)
	r := newGCTestRunner(t, dir, snap, reg, testLogger())
	r.now = func() time.Time { return now }
	st := &fakeGCStore{points: map[string]uint64{}}

	sum := r.pass(ctx, st)
	if len(st.deletes) != gcMaxDeletesPerPass {
		t.Fatalf("deletes = %d, want the %d cap", len(st.deletes), gcMaxDeletesPerPass)
	}
	for i, key := range oldestFirst[:gcMaxDeletesPerPass] {
		if st.deletes[i] != key {
			t.Fatalf("delete[%d] = %s, want %s (oldest first)", i, st.deletes[i], key)
		}
	}
	if got := sum.gcSuppressedCap.Load(); got != 5 {
		t.Errorf("suppressed_cap = %d, want 5", got)
	}
	if len(reg.Entries) != 6 { // 5 suppressed candidates + the alive repo
		t.Errorf("registry entries = %d, want 6", len(reg.Entries))
	}
}

// T140: dry-run counts the victims, logs would-delete and touches nothing.
func TestGCDryRun(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	dir := t.TempDir()
	mirror, state := makeMirror(t, dir, "grp", "fixture")
	reg := &gcRegistry{Entries: map[string]*gcEntry{
		gcKey("grp", "fixture"): {Namespace: "grp", Path: "fixture", LastSeenAt: now.Add(-time.Hour),
			FirstMissingAt: now.Add(-11 * time.Minute), MissStreak: 1},
		gcKey("grp", "other"): {Namespace: "grp", Path: "other", LastSeenAt: now},
	}}
	snap := &snapshot{}
	setSnap(snap, []provider.Repo{gcRepo("grp", "other")}, now)
	var buf bytes.Buffer
	r := newGCTestRunner(t, dir, snap, reg, slog.New(slog.NewTextHandler(&buf, nil)))
	r.cfg.Worker.GCDryRun = true
	r.now = func() time.Time { return now }
	st := &fakeGCStore{points: map[string]uint64{gcKey("grp", "fixture"): 7}}

	sum := r.pass(ctx, st)
	if len(st.deletes) != 0 {
		t.Fatalf("dry-run deleted points: %v", st.deletes)
	}
	if len(st.counts) != 1 {
		t.Errorf("dry-run counts = %v, want the read-back of the victim", st.counts)
	}
	if sum.gcDryRun.Load() != 1 {
		t.Errorf("gc_dry_run = %d, want 1", sum.gcDryRun.Load())
	}
	if !strings.Contains(buf.String(), "gc would-delete") || !strings.Contains(buf.String(), "points=7") {
		t.Errorf("log does not report the would-delete: %q", buf.String())
	}
	if _, ok := reg.Entries[gcKey("grp", "fixture")]; !ok {
		t.Error("dry-run removed the registry entry")
	}
	if _, err := os.Stat(mirror); err != nil {
		t.Errorf("dry-run touched the mirror: %v", err)
	}
	if _, err := os.Stat(state); err != nil {
		t.Errorf("dry-run touched the state: %v", err)
	}
}

// T140: a busy repository (held lock) is skipped and deleted on a later pass.
func TestGCBusyLockSkips(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	dir := t.TempDir()
	reg := &gcRegistry{Entries: map[string]*gcEntry{
		gcKey("grp", "fixture"): {Namespace: "grp", Path: "fixture", LastSeenAt: now.Add(-time.Hour),
			FirstMissingAt: now.Add(-11 * time.Minute), MissStreak: 2},
		gcKey("grp", "other"): {Namespace: "grp", Path: "other", LastSeenAt: now},
	}}
	snap := &snapshot{}
	setSnap(snap, []provider.Repo{gcRepo("grp", "other")}, now)
	r := newGCTestRunner(t, dir, snap, reg, testLogger())
	r.now = func() time.Time { return now }
	st := &fakeGCStore{}

	locked := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_ = withRepoLock(repoSlug("grp/fixture"), func() error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked

	sum := r.pass(ctx, st)
	if len(st.deletes) != 0 || sum.gcSuppressedBusy.Load() != 1 {
		t.Fatalf("busy pass: deletes=%v busy=%d, want none/1", st.deletes, sum.gcSuppressedBusy.Load())
	}
	if _, ok := reg.Entries[gcKey("grp", "fixture")]; !ok {
		t.Fatal("the busy entry was dropped")
	}
	close(release)
	<-finished // the lock is released before the next pass

	sum = r.pass(ctx, st)
	if len(st.deletes) != 1 || st.deletes[0] != gcKey("grp", "fixture") {
		t.Fatalf("free pass: deletes=%v, want grp/fixture", st.deletes)
	}
}

// T140: an empty registry never nukes anything; unmatched mirrors are orphan
// warnings only.
func TestGCEmptyRegistryNoNuke(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	dir := t.TempDir()
	mirror, _ := makeMirror(t, dir, "grp", "fixture")
	var buf bytes.Buffer
	reg := &gcRegistry{Entries: map[string]*gcEntry{}}
	snap := &snapshot{}
	r := newGCTestRunner(t, dir, snap, reg, slog.New(slog.NewTextHandler(&buf, nil)))
	r.now = func() time.Time { return now }
	st := &fakeGCStore{}

	sum := r.pass(ctx, st)
	if sum.gcAnomalies.Load() != 0 {
		t.Errorf("an empty registry + empty snapshot is not an anomaly: %d", sum.gcAnomalies.Load())
	}
	if len(st.deletes) != 0 || len(st.counts) != 0 {
		t.Errorf("store was touched: counts=%v deletes=%v", st.counts, st.deletes)
	}
	if _, err := os.Stat(mirror); err != nil {
		t.Errorf("mirror was touched: %v", err)
	}
	if !strings.Contains(buf.String(), "orphan mirror") {
		t.Errorf("unmatched mirror not warned: %q", buf.String())
	}
}

// T140: seeding adds mirror entries matched by a known namespace; an unknown
// namespace mirror is an orphan (no entry, no deletion).
func TestGCSeedMatchedMirrors(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	dir := t.TempDir()
	makeMirror(t, dir, "grp", "fixture")
	makeMirror(t, dir, "grp", "ghost")
	makeMirror(t, dir, "unknown", "stray")
	var buf bytes.Buffer
	reg := &gcRegistry{Entries: map[string]*gcEntry{}}
	snap := &snapshot{}
	setSnap(snap, []provider.Repo{gcRepo("grp", "fixture")}, now)
	r := newGCTestRunner(t, dir, snap, reg, slog.New(slog.NewTextHandler(&buf, nil)))
	r.now = func() time.Time { return now }
	st := &fakeGCStore{}

	r.pass(ctx, st)
	if e, ok := reg.Entries[gcKey("grp", "fixture")]; !ok || e.MissStreak != 0 {
		t.Errorf("seeded live entry = %+v, ok=%v", e, ok)
	}
	if e, ok := reg.Entries[gcKey("grp", "ghost")]; !ok || e.MissStreak != 1 {
		t.Errorf("mirror-only entry = %+v, ok=%v; want a grace start", e, ok)
	}
	if _, ok := reg.Entries[gcKey("unknown", "stray")]; ok {
		t.Error("an orphan mirror of an unknown namespace got an entry")
	}
	if len(st.deletes) != 0 {
		t.Errorf("seed pass deleted something: %v", st.deletes)
	}
	if !strings.Contains(buf.String(), "orphan mirror") {
		t.Errorf("orphan mirror warning missing: %q", buf.String())
	}
}

// T140: a count/delete error keeps the entry for the next pass.
func TestGCStoreErrorKeepsEntry(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	dir := t.TempDir()
	reg := &gcRegistry{Entries: map[string]*gcEntry{
		gcKey("grp", "fixture"): {Namespace: "grp", Path: "fixture", LastSeenAt: now.Add(-time.Hour),
			FirstMissingAt: now.Add(-11 * time.Minute), MissStreak: 2},
		gcKey("grp", "other"): {Namespace: "grp", Path: "other", LastSeenAt: now},
	}}
	snap := &snapshot{}
	setSnap(snap, []provider.Repo{gcRepo("grp", "other")}, now)
	r := newGCTestRunner(t, dir, snap, reg, testLogger())
	r.now = func() time.Time { return now }
	st := &fakeGCStore{countErr: context.DeadlineExceeded}

	sum := r.pass(ctx, st)
	if len(st.deletes) != 0 || sum.failed.Load() != 1 {
		t.Fatalf("error pass: deletes=%v errors=%d, want none/1", st.deletes, sum.failed.Load())
	}
	if _, ok := reg.Entries[gcKey("grp", "fixture")]; !ok {
		t.Error("the entry was dropped on a store error")
	}
}

// T140: the registry save/load round-trip is atomic and lossless.
func TestGCRegistryRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), gcRegistryName)
	reg := &gcRegistry{Entries: map[string]*gcEntry{
		gcKey("grp", "a"): {Namespace: "grp", Path: "a", LastSeenAt: time.Unix(1000, 0), MissStreak: 2},
	}}
	if err := reg.save(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("the tmp file remained after rename")
	}
	got, err := loadGCRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	e, ok := got.Entries[gcKey("grp", "a")]
	if !ok || e.MissStreak != 2 || !e.LastSeenAt.Equal(time.Unix(1000, 0)) {
		t.Errorf("round-trip entry = %+v, ok=%v", e, ok)
	}
	// a missing file is an empty registry, not an error.
	empty, err := loadGCRegistry(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil || len(empty.Entries) != 0 {
		t.Errorf("missing file = %+v, %v", empty, err)
	}
}

// two pads a small integer to two digits ("3" → "03").
func two(i int) string {
	if i < 10 {
		return "0" + string(rune('0'+i))
	}
	return string(rune('0'+i/10)) + string(rune('0'+i%10))
}
