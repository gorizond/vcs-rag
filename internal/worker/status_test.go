package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/gorizond/vcs-rag/internal/config"
	"github.com/gorizond/vcs-rag/internal/gitengine"
	"github.com/gorizond/vcs-rag/internal/provider"
	"github.com/gorizond/vcs-rag/internal/provider/fake"
)

// fakeStatusStore records the published KV entries.
type fakeStatusStore struct {
	puts map[string][]byte
	err  error
}

func (f *fakeStatusStore) Put(key string, value []byte) error {
	if f.err != nil {
		return f.err
	}
	if f.puts == nil {
		f.puts = map[string][]byte{}
	}
	f.puts[key] = value
	return nil
}

// fakePointCounter serves the points count for collectWorkerStatus.
type fakePointCounter struct {
	calls int
	n     uint64
	err   error
}

func (f *fakePointCounter) Count(context.Context) (uint64, error) {
	f.calls++
	return f.n, f.err
}

// mustSaveState writes a repository state sidecar.
func mustSaveState(t *testing.T, path string, st *MirrorState) {
	t.Helper()
	if st.Branches == nil {
		st.Branches = map[string]BranchState{}
	}
	if err := SaveState(path, st); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
}

// T150: the published payload marshals to the documented keys and round-trips.
func TestWorkerStatusPayloadRoundTrip(t *testing.T) {
	pts := uint64(7)
	st := &workerStatus{
		ReposTotal:    3,
		ReposIndexed:  1,
		ReposPending:  2,
		PendingSample: []string{"g/b", "g/c"},
		Points:        &pts,
		LastPass: &indexPassStats{
			Repos: 3, OK: 2, Errors: 1, IndexedCommits: 4,
			Resumed: 5, Skipped: 6, Pruned: 1, DurationMS: 42,
		},
		UpdatedAt: "2026-01-01T00:00:00Z",
	}
	data, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"repos_total":3`, `"repos_indexed":1`, `"repos_pending":2`,
		`"pending_sample":["g/b","g/c"]`, `"points":7`,
		`"indexed_commits":4`, `"duration_ms":42`, `"updated_at":"2026-01-01T00:00:00Z"`,
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("payload does not contain %s: %s", want, data)
		}
	}
	var got workerStatus
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.ReposTotal != 3 || got.ReposIndexed != 1 || got.ReposPending != 2 ||
		len(got.PendingSample) != 2 || got.Points == nil || *got.Points != 7 ||
		got.LastPass == nil || got.LastPass.IndexedCommits != 4 || got.LastPass.DurationMS != 42 {
		t.Errorf("round-trip = %+v", got)
	}

	// an unknown points count and no pass are omitted, never zeroed.
	bare, err := json.Marshal(&workerStatus{UpdatedAt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(bare), "points") || strings.Contains(string(bare), "last_pass") {
		t.Errorf("optional fields must be omitted: %s", bare)
	}
}

// T150: the publisher writes at most once per interval.
func TestStatusPublisherThrottle(t *testing.T) {
	store := &fakeStatusStore{}
	base := time.Unix(1000, 0)
	p := newStatusPublisher(store, testLogger())
	p.now = func() time.Time { return base }

	if !p.due() {
		t.Fatal("the first publication must be due")
	}
	if p.due() {
		t.Error("a second publication within the interval must be throttled")
	}
	base = base.Add(workerStatusInterval)
	if !p.due() {
		t.Error("a publication after the interval must be due")
	}

	p.publish(&workerStatus{ReposTotal: 1, PendingSample: []string{}, UpdatedAt: "x"})
	data, ok := store.puts[workerStatusKey]
	if !ok {
		t.Fatalf("no entry written, puts = %v", store.puts)
	}
	if !json.Valid(data) {
		t.Errorf("the published value is not valid JSON: %s", data)
	}

	// a store failure is logged, never fatal.
	store.err = errors.New("boom")
	p.publish(&workerStatus{})
}

// T150/T153: without NATS there is no reporter — the nil receiver is a no-op
// and every lane stays quiet.
func TestStatusReporterNoNATS(t *testing.T) {
	if rep := newStatusReporter(nil, RepoDeps{}, &snapshot{}); rep != nil {
		t.Errorf("newStatusReporter(nil) = %+v, want nil", rep)
	}
	var rep *statusReporter
	rep.recordPass(newSummary("index")) // must not panic
}

// newTestReporter builds a reporter over fakes (no NATS).
func newTestReporter(t *testing.T, store *fakeStatusStore, snap *snapshot, deps RepoDeps) (*statusReporter, *time.Time) {
	t.Helper()
	pub := newStatusPublisher(store, testLogger())
	base := time.Unix(1000, 0)
	pub.now = func() time.Time { return base }
	rep := newStatusReporter(pub, deps, snap)
	rep.points = &fakePointCounter{n: 5}
	return rep, &base
}

// T153 (a): an empty discovery snapshot never overwrites the previous value;
// the first publication happens on the next tick after the snapshot fills.
func TestStatusReporterEmptySnapshotGuard(t *testing.T) {
	ctx := context.Background()
	store := &fakeStatusStore{}
	deps := RepoDeps{VCSType: "gitlab", Engine: gitengine.New(t.TempDir()), Logger: testLogger()}
	snap := &snapshot{}
	rep, base := newTestReporter(t, store, snap, deps)

	rep.tick(ctx)
	if len(store.puts) != 0 {
		t.Fatalf("an empty snapshot published: %v", store.puts)
	}
	// another empty tick logs the debug line once (not asserted here) and still
	// publishes nothing.
	rep.tick(ctx)
	if len(store.puts) != 0 {
		t.Fatalf("an empty snapshot published on the second tick: %v", store.puts)
	}

	// the snapshot fills: the next tick publishes.
	snap.set([]provider.Repo{{ID: "1", Namespace: "g", Path: "a"}})
	*base = base.Add(workerStatusInterval)
	rep.tick(ctx)
	if len(store.puts) != 1 {
		t.Fatalf("puts = %d, want the first publication after the snapshot filled", len(store.puts))
	}
}

// T153 (b): the reporter publishes while a long pass is still running — before
// the first completed pass last_pass is omitted; afterwards it appears.
func TestStatusReporterTicksWithoutCompletedPass(t *testing.T) {
	ctx := context.Background()
	store := &fakeStatusStore{}
	deps := RepoDeps{VCSType: "gitlab", Engine: gitengine.New(t.TempDir()), Logger: testLogger()}
	snap := &snapshot{}
	snap.set([]provider.Repo{{ID: "1", Namespace: "g", Path: "a"}})
	rep, base := newTestReporter(t, store, snap, deps)

	rep.tick(ctx)
	if len(store.puts) != 1 {
		t.Fatalf("puts = %d, want a publication without a completed pass", len(store.puts))
	}
	if strings.Contains(string(store.puts[workerStatusKey]), `"last_pass"`) {
		t.Errorf("last_pass present before the first completed pass: %s", store.puts[workerStatusKey])
	}

	// a completed pass is recorded; the next tick carries its counters.
	sum := newSummary("index")
	sum.countRepo(nil)
	sum.addIndexed(3)
	rep.recordPass(sum)
	*base = base.Add(workerStatusInterval)
	rep.tick(ctx)
	if len(store.puts) != 1 {
		t.Fatalf("puts = %d, want the entry to be overwritten in place", len(store.puts))
	}
	if !strings.Contains(string(store.puts[workerStatusKey]), `"indexed_commits":3`) {
		t.Errorf("last_pass missing after the completed pass: %s", store.puts[workerStatusKey])
	}

	// T153 (c): the throttle still holds — a second tick within the interval
	// does not publish again (the same map entry is rewritten only when due).
	before := string(store.puts[workerStatusKey])
	rep.tick(ctx)
	if string(store.puts[workerStatusKey]) != before {
		t.Error("a tick within the interval republished")
	}
}

// T150: indexed/pending counters come from the state sidecars; the sample is
// capped and sorted; the pass counters and the points count are attached.
func TestCollectWorkerStatus(t *testing.T) {
	dir := t.TempDir()
	deps := RepoDeps{VCSType: "gitlab", Engine: gitengine.New(dir), Logger: testLogger()}
	indexed := provider.Repo{ID: "1", Namespace: "g", Path: "a"}
	emptyState := provider.Repo{ID: "2", Namespace: "g", Path: "b"}
	noState := provider.Repo{ID: "3", Namespace: "g", Path: "c"}
	mustSaveState(t, gitengine.StatePath(deps.Engine.MirrorPath("gitlab", indexed)), &MirrorState{
		Branches: map[string]BranchState{"main": {IndexedCommits: []string{"sha1"}}},
	})
	mustSaveState(t, gitengine.StatePath(deps.Engine.MirrorPath("gitlab", emptyState)), &MirrorState{
		Branches: map[string]BranchState{"main": {}},
	})
	// a corrupt sidecar counts as pending, not as indexed.
	corrupt := provider.Repo{ID: "4", Namespace: "g", Path: "d"}
	corruptPath := gitengine.StatePath(deps.Engine.MirrorPath("gitlab", corrupt))
	if err := os.MkdirAll(dir+"/gitlab/g", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(corruptPath, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}

	snap := &snapshot{}
	snap.set([]provider.Repo{indexed, emptyState, noState, corrupt})
	counter := &fakePointCounter{n: 42}
	now := time.Unix(1000, 0)

	st := collectWorkerStatus(context.Background(), deps, snap, nil, counter, now)
	if st.ReposTotal != 4 || st.ReposIndexed != 1 || st.ReposPending != 3 {
		t.Fatalf("totals = %d/%d/%d, want 4/1/3", st.ReposTotal, st.ReposIndexed, st.ReposPending)
	}
	if strings.Join(st.PendingSample, ",") != "g/b,g/c,g/d" {
		t.Errorf("pending sample = %v, want the sorted pending names", st.PendingSample)
	}
	if st.Points == nil || *st.Points != 42 || counter.calls != 1 {
		t.Errorf("points = %v (calls=%d), want 42 via one call", st.Points, counter.calls)
	}
	if st.LastPass != nil {
		t.Errorf("last_pass without a pass = %+v, want omitted", st.LastPass)
	}
	if st.UpdatedAt != now.UTC().Format(time.RFC3339) {
		t.Errorf("updated_at = %q", st.UpdatedAt)
	}

	// the pass counters are attached when a pass happened.
	sum := newSummary("index")
	sum.countRepo(nil)
	sum.addIndexed(3)
	sum.addResumed(1)
	sum.addPruned(2)
	st = collectWorkerStatus(context.Background(), deps, snap, passStats(sum), counter, now)
	if st.LastPass == nil || st.LastPass.Repos != 1 || st.LastPass.OK != 1 ||
		st.LastPass.IndexedCommits != 3 || st.LastPass.Resumed != 1 || st.LastPass.Pruned != 2 {
		t.Errorf("last_pass = %+v", st.LastPass)
	}

	// a failed points count is omitted.
	st = collectWorkerStatus(context.Background(), deps, snap, nil, &fakePointCounter{err: errors.New("boom")}, now)
	if st.Points != nil {
		t.Errorf("points = %v, want omitted on error", st.Points)
	}

	// the sample is capped at 10 and the pending count stays complete.
	var many []provider.Repo
	for i := 0; i < 12; i++ {
		many = append(many, provider.Repo{ID: "x", Namespace: "g", Path: "p" + string(rune('a'+i))})
	}
	snap.set(many)
	st = collectWorkerStatus(context.Background(), deps, snap, nil, counter, now)
	if st.ReposPending != 12 || len(st.PendingSample) != workerStatusSampleMax {
		t.Errorf("pending = %d sample = %d, want 12/%d", st.ReposPending, len(st.PendingSample), workerStatusSampleMax)
	}
}

// T150: the status store creates worker_status_<id> (create-if-missing) and
// writes the "status" key; the refresh_status bucket stays untouched.
func TestNatsStatusStoreEmbedded(t *testing.T) {
	url := startEmbeddedNATS(t)
	names := config.RefreshNames("status")
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()

	store := &natsStatusStore{nc: nc, names: names}
	if err := store.Put(workerStatusKey, []byte(`{"repos_total":1}`)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	js, err := nc.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	kv, err := js.KeyValue(names.WorkerKVBucket)
	if err != nil {
		t.Fatalf("bucket %s: %v", names.WorkerKVBucket, err)
	}
	entry, err := kv.Get(workerStatusKey)
	if err != nil {
		t.Fatalf("Get(%s): %v", workerStatusKey, err)
	}
	if !strings.Contains(string(entry.Value()), `"repos_total":1`) {
		t.Errorf("value = %s", entry.Value())
	}
	// the refresh status bucket belongs to the refresh topology only.
	if _, err := js.KeyValue(names.KVBucket); err == nil {
		t.Errorf("the status store created %s, want it untouched", names.KVBucket)
	}
	// a second Put reuses the ensured bucket.
	if err := store.Put(workerStatusKey, []byte(`{"repos_total":2}`)); err != nil {
		t.Fatalf("second Put: %v", err)
	}
}

// T029/T030 (FR-015): with VCS_DEDUP_ACCOUNT=1 the pass report warns when the
// authenticated account budget is >80% consumed — headers first, the instance
// page count as the header-less fallback, never a shared counter.
func TestSharedAccountWarn(t *testing.T) {
	newRep := func(usage float64, usageOK bool, pages int64) (*statusReporter, *summary) {
		deps := RepoDeps{Cfg: &config.Config{}, Logger: testLogger()}
		deps.Cfg.Worker.SharedAccountWarn = true
		deps.VCS = &fake.Provider{AccountUsage: usage, AccountUsageOK: usageOK, PageCount: pages}
		return newStatusReporter(newStatusPublisher(&fakeStatusStore{}, testLogger()), deps, &snapshot{}), newSummary("index")
	}

	rep, sum := newRep(0.9, true, 0)
	rep.recordPass(sum)
	if rep.lastPass == nil || !rep.lastPass.SharedAccountWarn {
		t.Fatalf("last_pass = %+v, want shared_account_warn", rep.lastPass)
	}
	data, err := json.Marshal(rep.lastPass)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"shared_account_warn":true`) {
		t.Errorf("report does not carry the flag: %s", data)
	}

	// below the threshold — no warning.
	rep, sum = newRep(0.5, true, 0)
	rep.recordPass(sum)
	if rep.lastPass.SharedAccountWarn {
		t.Error("a 50% usage must not warn")
	}

	// no headers: the instance page count is compared with 5000/h.
	rep, sum = newRep(0, false, 4100)
	rep.recordPass(sum)
	if !rep.lastPass.SharedAccountWarn {
		t.Error("the header-less fallback did not warn above 80%")
	}

	// the feature is off by default (VCS_DEDUP_ACCOUNT unset).
	deps := RepoDeps{Cfg: &config.Config{}, Logger: testLogger()}
	deps.VCS = &fake.Provider{AccountUsage: 0.99, AccountUsageOK: true}
	rep = newStatusReporter(newStatusPublisher(&fakeStatusStore{}, testLogger()), deps, &snapshot{})
	rep.recordPass(newSummary("index"))
	if rep.lastPass.SharedAccountWarn {
		t.Error("the warning must be gated by Worker.SharedAccountWarn")
	}
}

// T038 (FR-006/FR-008): neither the published worker status nor the pass
// report may carry token values or credential-bearing URLs. The hostile repo
// record below has credentials in CloneURL — the status must not copy it.
func TestStatusReportHasNoCredentials(t *testing.T) {
	const marker = "glpat-STATUS-CANARY"
	deps := RepoDeps{VCSType: "gitlab", Engine: gitengine.New(t.TempDir()), Logger: testLogger()}
	deps.Cfg = &config.Config{}
	deps.Cfg.Provider.GitLabToken = marker
	snap := &snapshot{}
	snap.set([]provider.Repo{{
		ID: "1", Namespace: "grp", Path: "fixture",
		CloneURL: "https://oauth2:" + marker + "@gitlab.example/grp/fixture.git",
	}})
	sum := newSummary("index")
	sum.configureUnitBudget(0)
	sum.markUnprocessed("grp/fixture")

	status, err := json.Marshal(collectWorkerStatus(context.Background(), deps, snap, passStats(sum), nil, time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	pass, err := json.Marshal(passStats(sum))
	if err != nil {
		t.Fatal(err)
	}
	credURL := regexp.MustCompile(`://[^/]*:[^/]*@`)
	for name, blob := range map[string]string{"status": string(status), "pass_report": string(pass)} {
		if strings.Contains(blob, marker) {
			t.Errorf("%s leaked the token: %s", name, blob)
		}
		if credURL.MatchString(blob) {
			t.Errorf("%s contains a credential-bearing URL: %s", name, blob)
		}
	}
}
