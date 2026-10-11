package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/qdrant/go-client/qdrant"

	"github.com/gorizond/vcs-rag/internal/config"
	"github.com/gorizond/vcs-rag/internal/embed"
	"github.com/gorizond/vcs-rag/internal/gitengine"
	"github.com/gorizond/vcs-rag/internal/provider"
	"github.com/gorizond/vcs-rag/internal/provider/fake"
	"github.com/gorizond/vcs-rag/internal/store"
)

// Golden IDs match the values in internal/store/payload_test.go
// (an independent python check of the uuid5 formula) — mapping must not lose fields.

func TestMapIssue(t *testing.T) {
	it := mapIssue("github", "org", "myrepo", provider.Issue{
		ID: 7, Title: "Header", Body: "Body", State: "open",
		Author: "alice", Assignees: []string{"bob"}, Labels: []string{"bug"}, Milestone: "v1",
	})
	if it.ID != "79eb2957-ed8a-592d-99e5-4b0b5e1cda64" {
		t.Errorf("ID = %q", it.ID)
	}
	want := map[string]any{
		"vcs_type": "github", "group_org": "org", "repo": "myrepo", "type": "issue",
		"issue_id": int64(7), "state": "open", "text": "Header\n\nBody",
		"author": "alice", "assignees": []string{"bob"}, "reviewers": []string(nil),
		"labels": []string{"bug"}, "milestone": "v1",
	}
	if !reflect.DeepEqual(it.Payload, want) {
		t.Errorf("payload = %v, want %v", it.Payload, want)
	}
	// MetaPayload is the F3 subset (set_payload on unchanged points, T029).
	wantMeta := map[string]any{
		"author": "alice", "assignees": []string{"bob"}, "reviewers": []string(nil),
		"labels": []string{"bug"}, "milestone": "v1", "state": "open",
	}
	if !reflect.DeepEqual(it.MetaPayload, wantMeta) {
		t.Errorf("meta payload = %v, want %v", it.MetaPayload, wantMeta)
	}
	if it.Text != "Header\n\nBody" {
		t.Errorf("text = %q", it.Text)
	}

	// empty body — title only
	bare := mapIssue("github", "org", "myrepo", provider.Issue{ID: 1, Title: "Only header"})
	if bare.Text != "Only header" {
		t.Errorf("text without body = %q", bare.Text)
	}
}

func TestMapChangeRequest(t *testing.T) {
	it := mapChangeRequest("gitlab", "grp", "myrepo", provider.ChangeRequest{
		ID: 42, Title: "PR header", Body: "PR body", State: "opened",
		SourceBranch: "feat", TargetBranch: "main",
		Author: "carol", Assignees: []string{"dave"}, Reviewers: []string{"erin"},
		Labels: []string{"review"}, Milestone: "v2",
	})
	if it.ID != "bd8787b5-9878-55f0-a044-7f60c392e290" {
		t.Errorf("ID = %q", it.ID)
	}
	want := map[string]any{
		"vcs_type": "gitlab", "group_org": "grp", "repo": "myrepo", "type": "pull_request",
		"pr_id": int64(42), "source_branch": "feat", "target_branch": "main",
		"state": "opened", "text": "PR header\n\nPR body",
		"author": "carol", "assignees": []string{"dave"}, "reviewers": []string{"erin"},
		"labels": []string{"review"}, "milestone": "v2",
	}
	if !reflect.DeepEqual(it.Payload, want) {
		t.Errorf("payload = %v, want %v", it.Payload, want)
	}
}

func TestMapCIFailureTail(t *testing.T) {
	var log strings.Builder
	for i := 1; i <= 100; i++ {
		fmt.Fprintf(&log, "line %d\n", i)
	}
	p := provider.Pipeline{ID: "100", Ref: "main", SHA: "abc"}
	j := provider.Job{ID: "1000", Name: "build", Status: "failed"}

	it := mapCIFailure("gitlab", "grp", "myrepo", p, j, log.String())
	if it.ID != "df747038-0516-562a-afc9-f7d6b1e2a8ff" {
		t.Errorf("ID = %q", it.ID)
	}
	for key, want := range map[string]any{
		"vcs_type": "gitlab", "group_org": "grp", "repo": "myrepo", "type": "ci_failure",
		"pipeline_id": "100", "job_id": "1000", "job_name": "build",
		"branch": "main", "commit_sha": "abc",
	} {
		if it.Payload[key] != want {
			t.Errorf("payload[%q] = %v, want %v", key, it.Payload[key], want)
		}
	}

	// text = title + ≤ ciTailLines log lines (the tail covers 51..100)
	lines := strings.Split(it.Text, "\n")
	if len(lines) > 1+ciTailLines {
		t.Errorf("lines in text %d, want ≤ %d", len(lines), 1+ciTailLines)
	}
	if !strings.Contains(lines[0], "build") || !strings.Contains(lines[0], "100") {
		t.Errorf("title = %q", lines[0])
	}
	if !strings.HasSuffix(it.Text, "line 100") {
		t.Error("the tail does not end with the last log line")
	}
	if strings.Contains(it.Text, "line 50") {
		t.Error("a line outside the last 50 got into the tail")
	}

	// empty log — title only
	bare := mapCIFailure("gitlab", "grp", "myrepo", p, j, "")
	if strings.Contains(bare.Text, "\n") {
		t.Errorf("text without log = %q", bare.Text)
	}
}

func TestJobFailed(t *testing.T) {
	if !jobFailed(provider.Job{Status: "failed"}) {
		t.Error("GitLab status=failed not recognized")
	}
	if !jobFailed(provider.Job{Conclusion: "failure"}) {
		t.Error("GitHub conclusion=failure not recognized")
	}
	if jobFailed(provider.Job{Status: "success", Conclusion: "success"}) {
		t.Error("success treated as a failure")
	}
}

// --- T080: incremental metadata (skip unchanged, CI once) ---

// countingEmbed is an embed.Provider double: counts calls and texts.
type countingEmbed struct {
	calls int
	texts int
}

func (c *countingEmbed) Dim() int { return 4 }

func (c *countingEmbed) Embed(_ context.Context, texts []string) ([][]float32, error) {
	c.calls++
	c.texts += len(texts)
	vecs := make([][]float32, len(texts))
	for i := range vecs {
		vecs[i] = []float32{1, 0, 0, 0}
	}
	return vecs, nil
}

// countingStore is a metaStore double: records upserts/deletes, serves the
// configured existing ci points for prune.
type countingStore struct {
	upserts    [][]store.Point
	deletes    [][2]string
	deletedIDs []string
	existing   []store.ScoredPoint
	payloads   []payloadCall
}

// payloadCall — one SetPayload call (T029).
type payloadCall struct {
	ids     []string
	payload map[string]*qdrant.Value
}

func (c *countingStore) Upsert(_ context.Context, points []store.Point) error {
	c.upserts = append(c.upserts, points)
	return nil
}

// SetPayload records the F3 payload-only updates (T029).
func (c *countingStore) SetPayload(_ context.Context, ids []string, payload map[string]*qdrant.Value) error {
	c.payloads = append(c.payloads, payloadCall{ids: ids, payload: payload})
	return nil
}

// DeleteByIDs records the entity-reconcile deletions (T140).
func (c *countingStore) DeleteByIDs(_ context.Context, ids []string) error {
	c.deletedIDs = append(c.deletedIDs, ids...)
	return nil
}

func (c *countingStore) ScrollCIFailures(_ context.Context, _ string, _ uint32) ([]store.ScoredPoint, error) {
	return c.existing, nil
}

func (c *countingStore) DeleteCIFailure(_ context.Context, _, pipelineID, jobID string) error {
	c.deletes = append(c.deletes, [2]string{pipelineID, jobID})
	return nil
}

// points returns every upserted point.
func (c *countingStore) points() []store.Point {
	var out []store.Point
	for _, batch := range c.upserts {
		out = append(out, batch...)
	}
	return out
}

// newMetaDeps builds RepoDeps for a metadata pass without Qdrant (Store is
// passed to processMetadataOnce separately; the engine only resolves the state path).
func newMetaDeps(t *testing.T, vcs provider.VCSProvider, emb embed.Provider, logger *slog.Logger) RepoDeps {
	t.Helper()
	return RepoDeps{
		VCSType: "gitlab",
		VCS:     vcs,
		Engine:  gitengine.New(t.TempDir()),
		Embed:   emb,
		Logger:  logger,
	}
}

// T080 (a)/(b)/(c): the first pass is full; an unchanged pass re-embeds
// nothing (skipped=all); a changed item is embedded alone.
func TestMetadataIncrementalIssuesPRs(t *testing.T) {
	ctx := context.Background()
	t0 := time.Unix(1000, 0)
	repo := provider.Repo{ID: "1", Namespace: "g", Path: "r"}
	vcs := &fake.Provider{
		Repos: []provider.Repo{repo},
		Issues: map[string][]provider.Issue{
			"1": {{ID: 1, Title: "Issue", UpdatedAt: t0}},
		},
		ChangeRequests: map[string][]provider.ChangeRequest{
			"1": {{ID: 2, Title: "PR", UpdatedAt: t0}},
		},
	}
	emb := &countingEmbed{}
	var logBuf bytes.Buffer
	deps := newMetaDeps(t, vcs, emb, slog.New(slog.NewTextHandler(&logBuf, nil)))
	st := &countingStore{}
	statePath := gitengine.StatePath(deps.Engine.MirrorPath(deps.VCSType, repo))

	// (a) first pass (empty state) — everything is embedded and written.
	if err := processMetadataOnce(ctx, deps, repo, st); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if emb.texts != 2 || len(st.points()) != 2 {
		t.Errorf("first pass: embed texts = %d, upserted = %d, want 2/2", emb.texts, len(st.points()))
	}
	saved, err := LoadState(statePath)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if saved.Meta["issue:1"] != t0.Unix() || saved.Meta["mr:2"] != t0.Unix() {
		t.Errorf("state meta = %v, want issue:1/mr:2 = %d", saved.Meta, t0.Unix())
	}

	// (b) unchanged pass — no re-embed, no upsert, skipped=2 in the log.
	embTexts, upserted := emb.texts, len(st.points())
	logBuf.Reset()
	if err := processMetadataOnce(ctx, deps, repo, st); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if emb.texts != embTexts || len(st.points()) != upserted {
		t.Errorf("unchanged pass: embed texts %d→%d, points %d→%d, want no growth",
			embTexts, emb.texts, upserted, len(st.points()))
	}
	if !strings.Contains(logBuf.String(), "skipped=2") {
		t.Errorf("log does not report skipped=2: %q", logBuf.String())
	}

	// (c) one changed item (updated_at++) — only it is embedded.
	vcs.ChangeRequests["1"] = []provider.ChangeRequest{
		{ID: 2, Title: "PR v2", UpdatedAt: t0.Add(time.Second)},
	}
	if err := processMetadataOnce(ctx, deps, repo, st); err != nil {
		t.Fatalf("third pass: %v", err)
	}
	if emb.texts != embTexts+1 {
		t.Errorf("changed pass: embed texts = %d, want %d", emb.texts, embTexts+1)
	}
	got := st.points()
	if len(got) != upserted+1 || got[len(got)-1].ID != store.PullRequestID("gitlab", "r", 2) {
		t.Errorf("changed pass: upserted %d points, want the changed PR only", len(got)-upserted)
	}
	saved, err = LoadState(statePath)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if saved.Meta["mr:2"] != t0.Add(time.Second).Unix() {
		t.Errorf("state mr:2 = %d, want %d", saved.Meta["mr:2"], t0.Add(time.Second).Unix())
	}
}

// T029/F3: the budget invariant — a pass that re-embeds nothing must still
// refresh the additive fields, and it does so via set_payload only.
func TestMetadataF3SetPayloadOnly(t *testing.T) {
	ctx := context.Background()
	t0 := time.Unix(1000, 0)
	repo := provider.Repo{ID: "1", Namespace: "g", Path: "r"}
	vcs := &fake.Provider{
		Repos: []provider.Repo{repo},
		Issues: map[string][]provider.Issue{
			"1": {{ID: 1, Title: "Issue", UpdatedAt: t0, Author: "alice", State: "open"}},
		},
		ChangeRequests: map[string][]provider.ChangeRequest{
			"1": {{ID: 2, Title: "PR", UpdatedAt: t0, Author: "bob", State: "merged"}},
		},
	}
	emb := &countingEmbed{}
	deps := newMetaDeps(t, vcs, emb, testLogger())
	st := &countingStore{}

	// first pass: both points are new — embedded and upserted with the F3
	// fields in the upsert payload; set_payload is not called.
	if err := processMetadataOnce(ctx, deps, repo, st); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if emb.texts != 2 || len(st.points()) != 2 || len(st.payloads) != 0 {
		t.Fatalf("first pass: texts=%d points=%d set_payload=%d, want 2/2/0",
			emb.texts, len(st.points()), len(st.payloads))
	}

	// second pass: nothing changed — no re-embed, no re-upsert; both points get
	// the F3 fields via set_payload.
	if err := processMetadataOnce(ctx, deps, repo, st); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if emb.texts != 2 || len(st.points()) != 2 {
		t.Errorf("second pass re-embedded: texts=%d points=%d", emb.texts, len(st.points()))
	}
	if len(st.payloads) != 2 {
		t.Fatalf("second pass set_payload = %d, want 2", len(st.payloads))
	}
	states := map[string]string{}
	for _, call := range st.payloads {
		if len(call.ids) != 1 {
			t.Errorf("set_payload ids = %v, want one point", call.ids)
			continue
		}
		if _, ok := call.payload["author"]; !ok {
			t.Errorf("set_payload misses the F3 fields: %v", call.payload)
		}
		states[call.ids[0]] = call.payload["state"].GetStringValue()
	}
	if states[store.IssueID("gitlab", "r", 1)] != "open" ||
		states[store.PullRequestID("gitlab", "r", 2)] != "merged" {
		t.Errorf("set_payload states = %v", states)
	}
}

// T080 (d): a failed CI job is embedded once; a rerun (new job id) is
// embedded again; prune removes the point and its state key.
func TestMetadataIncrementalCIOnce(t *testing.T) {
	ctx := context.Background()
	repo := provider.Repo{ID: "1", Namespace: "g", Path: "r"}
	failed := provider.Pipeline{ID: "100", Status: "failed", Ref: "main", SHA: "abc",
		Jobs: []provider.Job{{ID: "1000", Name: "build", Status: "failed"}}}
	vcs := &fake.Provider{
		Repos:     []provider.Repo{repo},
		Pipelines: map[string][]provider.Pipeline{"1": {failed}},
		Logs:      map[string]string{"1000": "boom\n"},
	}
	emb := &countingEmbed{}
	deps := newMetaDeps(t, vcs, emb, testLogger())
	st := &countingStore{}
	statePath := gitengine.StatePath(deps.Engine.MirrorPath(deps.VCSType, repo))

	if err := processMetadataOnce(ctx, deps, repo, st); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if emb.texts != 1 || len(st.points()) != 1 {
		t.Fatalf("first pass: embed texts = %d, points = %d, want 1/1", emb.texts, len(st.points()))
	}
	saved, err := LoadState(statePath)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if saved.Meta["ci:100:1000"] != 1 {
		t.Errorf("state ci:100:1000 = %d, want 1", saved.Meta["ci:100:1000"])
	}

	// repeated pass: the same job is not re-embedded.
	if err := processMetadataOnce(ctx, deps, repo, st); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if emb.texts != 1 || len(st.points()) != 1 {
		t.Errorf("repeat: embed texts = %d, points = %d, want unchanged", emb.texts, len(st.points()))
	}

	// a rerun gets a new job id → embedded once more.
	vcs.Pipelines["1"] = []provider.Pipeline{{ID: "100", Status: "failed", Ref: "main", SHA: "abc",
		Jobs: []provider.Job{{ID: "1001", Name: "build", Status: "failed"}}}}
	if err := processMetadataOnce(ctx, deps, repo, st); err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if emb.texts != 2 || len(st.points()) != 2 {
		t.Errorf("rerun: embed texts = %d, points = %d, want 2/2", emb.texts, len(st.points()))
	}

	// the window no longer has failures and the old point exists → prune + state cleanup.
	st.existing = []store.ScoredPoint{{Payload: map[string]any{"pipeline_id": "100", "job_id": "1000"}}}
	vcs.Pipelines["1"] = []provider.Pipeline{{ID: "100", Status: "success", Ref: "main", SHA: "abc"}}
	if err := processMetadataOnce(ctx, deps, repo, st); err != nil {
		t.Fatalf("prune pass: %v", err)
	}
	if len(st.deletes) != 1 || st.deletes[0] != [2]string{"100", "1000"} {
		t.Errorf("prune deletes = %v, want 100/1000", st.deletes)
	}
	saved, err = LoadState(statePath)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(saved.Meta) != 0 {
		t.Errorf("state ci keys not pruned: %v", saved.Meta)
	}
}

// T090: a failure between the issues/MR and CI phases keeps the first-phase
// checkpoint; the retry skips the written items and completes the CI phase.
func TestMetadataCheckpointBetweenPhases(t *testing.T) {
	ctx := context.Background()
	t0 := time.Unix(1000, 0)
	repo := provider.Repo{ID: "1", Namespace: "g", Path: "r"}
	vcs := &fake.Provider{
		Repos: []provider.Repo{repo},
		Issues: map[string][]provider.Issue{
			"1": {{ID: 1, Title: "Issue", UpdatedAt: t0}},
		},
		ChangeRequests: map[string][]provider.ChangeRequest{
			"1": {{ID: 2, Title: "PR", UpdatedAt: t0}},
		},
		Pipelines: map[string][]provider.Pipeline{
			"1": {{ID: "100", Status: "failed", Ref: "main", SHA: "abc",
				Jobs: []provider.Job{{ID: "1000", Name: "build", Status: "failed"}}}},
		},
		Logs:             map[string]string{"1000": "boom\n"},
		ErrListPipelines: errors.New("injected pipelines failure"),
	}
	emb := &countingEmbed{}
	deps := newMetaDeps(t, vcs, emb, testLogger())
	stats := newSummary("metadata")
	deps.stats = stats
	st := &countingStore{}
	statePath := gitengine.StatePath(deps.Engine.MirrorPath(deps.VCSType, repo))

	// 1) the CI phase fails after the issues/MR checkpoint.
	if err := processMetadataOnce(ctx, deps, repo, st); err == nil {
		t.Fatal("expected the injected pipelines failure")
	}
	if emb.texts != 2 {
		t.Errorf("embeddings before the failure = %d, want 2 (issues/MR only)", emb.texts)
	}
	saved, err := LoadState(statePath)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if saved.Meta["issue:1"] != t0.Unix() || saved.Meta["mr:2"] != t0.Unix() {
		t.Fatalf("issues/MR checkpoint not persisted: %v", saved.Meta)
	}

	// 2) retry: issues/MR are skipped (no re-embedding), the CI phase is written.
	vcs.ErrListPipelines = nil
	if err := processMetadataOnce(ctx, deps, repo, st); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if emb.texts != 3 {
		t.Errorf("embeddings after the retry = %d, want 3 (issues/MR not re-embedded)", emb.texts)
	}
	saved, err = LoadState(statePath)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if saved.Meta["ci:100:1000"] != 1 {
		t.Errorf("CI checkpoint missing: %v", saved.Meta)
	}
	if got := stats.skipped.Load(); got != 2 {
		t.Errorf("summary skipped = %d, want 2 (issues/MR)", got)
	}
}

// --- T140: entity reconcile (issue/MR GC in the metadata pass) ---

// reconcileDeps builds a metadata pass with an explicit GC mode (the real
// deletion mode is normally reachable only with WORKER_GC_DRY_RUN=false).
func reconcileDeps(t *testing.T, vcs provider.VCSProvider, logger *slog.Logger, dry bool) RepoDeps {
	t.Helper()
	deps := newMetaDeps(t, vcs, &countingEmbed{}, logger)
	deps.Cfg = &config.Config{Worker: config.Worker{GCDryRun: dry}}
	return deps
}

// T140: an issue absent twice in a row is deleted by its point ID; a returned
// one resets the miss counter; the MR is never touched.
func TestReconcileEntityDeleteAndReset(t *testing.T) {
	ctx := context.Background()
	t0 := time.Unix(1000, 0)
	repo := provider.Repo{ID: "1", Namespace: "g", Path: "r"}
	issue := provider.Issue{ID: 1, Title: "I", UpdatedAt: t0}
	vcs := &fake.Provider{
		Repos:          []provider.Repo{repo},
		Issues:         map[string][]provider.Issue{"1": {issue}},
		ChangeRequests: map[string][]provider.ChangeRequest{"1": {{ID: 2, Title: "PR", UpdatedAt: t0}}},
	}
	deps := reconcileDeps(t, vcs, testLogger(), false)
	st := &countingStore{}
	statePath := gitengine.StatePath(deps.Engine.MirrorPath(deps.VCSType, repo))
	pass := func() *MirrorState {
		t.Helper()
		if err := processMetadataOnce(ctx, deps, repo, st); err != nil {
			t.Fatalf("metadata pass: %v", err)
		}
		saved, err := LoadState(statePath)
		if err != nil {
			t.Fatalf("LoadState: %v", err)
		}
		return saved
	}

	pass() // full pass: issue:1 + mr:2
	// the issue disappears; the MR stays (its counter must reset, not grow).
	vcs.Issues["1"] = nil
	saved := pass()
	if got := saved.MetaMiss["issue:1"]; got != 1 {
		t.Fatalf("miss after one absent pass = %d, want 1", got)
	}
	if len(st.deletedIDs) != 0 {
		t.Fatalf("deleted before the threshold: %v", st.deletedIDs)
	}
	if _, ok := saved.Meta["issue:1"]; !ok {
		t.Fatal("the Meta key disappeared before the threshold")
	}

	// the issue returns → the counter resets.
	vcs.Issues["1"] = []provider.Issue{issue}
	saved = pass()
	if _, ok := saved.MetaMiss["issue:1"]; ok {
		t.Fatalf("returned issue not reset: %v", saved.MetaMiss)
	}

	// gone again: pass 4 = miss 1, pass 5 = miss 2 → delete by point ID.
	vcs.Issues["1"] = nil
	pass()
	saved = pass()
	want := store.IssueID("gitlab", "r", 1)
	if len(st.deletedIDs) != 1 || st.deletedIDs[0] != want {
		t.Fatalf("deleted IDs = %v, want [%s]", st.deletedIDs, want)
	}
	if _, ok := saved.Meta["issue:1"]; ok {
		t.Error("the Meta key of the deleted issue remained")
	}
	if _, ok := saved.MetaMiss["issue:1"]; ok {
		t.Error("the miss counter of the deleted issue remained")
	}
	if _, ok := saved.Meta["mr:2"]; !ok {
		t.Error("the MR key was touched by the issue reconcile")
	}
}

// T140: CI keys belong to the existing prune — the entity reconcile never
// counts or deletes them.
func TestReconcileCIUntouched(t *testing.T) {
	ctx := context.Background()
	repo := provider.Repo{ID: "1", Namespace: "g", Path: "r"}
	failed := provider.Pipeline{ID: "100", Status: "failed", Ref: "main", SHA: "abc",
		Jobs: []provider.Job{{ID: "1000", Name: "build", Status: "failed"}}}
	vcs := &fake.Provider{
		Repos:     []provider.Repo{repo},
		Pipelines: map[string][]provider.Pipeline{"1": {failed}},
		Logs:      map[string]string{"1000": "boom\n"},
	}
	deps := reconcileDeps(t, vcs, testLogger(), false)
	st := &countingStore{}
	statePath := gitengine.StatePath(deps.Engine.MirrorPath(deps.VCSType, repo))

	for i := 0; i < 3; i++ {
		if err := processMetadataOnce(ctx, deps, repo, st); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	saved, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := saved.Meta["ci:100:1000"]; !ok {
		t.Error("the CI key was removed by the entity reconcile")
	}
	for key := range saved.MetaMiss {
		if strings.HasPrefix(key, "ci:") {
			t.Errorf("a CI key entered meta_miss: %s", key)
		}
	}
	if len(st.deletedIDs) != 0 {
		t.Errorf("the reconcile deleted CI points: %v", st.deletedIDs)
	}
}

// T140: dry-run logs would-delete, keeps the keys and never calls the store.
func TestReconcileDryRun(t *testing.T) {
	ctx := context.Background()
	t0 := time.Unix(1000, 0)
	repo := provider.Repo{ID: "1", Namespace: "g", Path: "r"}
	vcs := &fake.Provider{
		Repos:  []provider.Repo{repo},
		Issues: map[string][]provider.Issue{"1": {{ID: 1, Title: "I", UpdatedAt: t0}}},
	}
	var buf bytes.Buffer
	deps := reconcileDeps(t, vcs, slog.New(slog.NewTextHandler(&buf, nil)), true)
	st := &countingStore{}
	statePath := gitengine.StatePath(deps.Engine.MirrorPath(deps.VCSType, repo))

	run := func() *MirrorState {
		t.Helper()
		if err := processMetadataOnce(ctx, deps, repo, st); err != nil {
			t.Fatalf("metadata pass: %v", err)
		}
		saved, err := LoadState(statePath)
		if err != nil {
			t.Fatal(err)
		}
		return saved
	}
	run() // index the issue
	vcs.Issues["1"] = nil
	run()          // miss 1
	saved := run() // miss 2 → would-delete
	if len(st.deletedIDs) != 0 {
		t.Fatalf("dry-run deleted points: %v", st.deletedIDs)
	}
	if saved.MetaMiss["issue:1"] != 2 {
		t.Errorf("miss = %d, want 2", saved.MetaMiss["issue:1"])
	}
	if _, ok := saved.Meta["issue:1"]; !ok {
		t.Error("dry-run dropped the Meta key")
	}
	if !strings.Contains(buf.String(), "reconcile would-delete") ||
		!strings.Contains(buf.String(), "issues=1") {
		t.Errorf("dry-run log missing the would-delete: %q", buf.String())
	}
}

// T140: the per-repository cap is 32 candidates; the suppression is counted.
func TestReconcilePerRepoCap(t *testing.T) {
	ctx := context.Background()
	t0 := time.Unix(1000, 0)
	repo := provider.Repo{ID: "1", Namespace: "g", Path: "r"}
	issues := make([]provider.Issue, 0, 40)
	for i := int64(1); i <= 40; i++ {
		issues = append(issues, provider.Issue{ID: i, Title: "I", UpdatedAt: t0})
	}
	vcs := &fake.Provider{
		Repos:  []provider.Repo{repo},
		Issues: map[string][]provider.Issue{"1": issues},
	}
	deps := reconcileDeps(t, vcs, testLogger(), false)
	stats := newSummary("metadata")
	deps.stats = stats
	st := &countingStore{}
	statePath := gitengine.StatePath(deps.Engine.MirrorPath(deps.VCSType, repo))

	if err := processMetadataOnce(ctx, deps, repo, st); err != nil {
		t.Fatal(err)
	}
	vcs.Issues["1"] = nil
	if err := processMetadataOnce(ctx, deps, repo, st); err != nil {
		t.Fatal(err)
	}
	if err := processMetadataOnce(ctx, deps, repo, st); err != nil {
		t.Fatal(err)
	}
	if len(st.deletedIDs) != metaDeletePerRepo {
		t.Fatalf("deleted = %d, want the per-repo cap %d", len(st.deletedIDs), metaDeletePerRepo)
	}
	saved, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Meta) != 8 || len(saved.MetaMiss) != 8 {
		t.Errorf("remaining keys = %d/%d, want 8/8", len(saved.Meta), len(saved.MetaMiss))
	}
	if got := stats.metaSuppressedCap.Load(); got != 8 {
		t.Errorf("suppressed_cap = %d, want 8", got)
	}
	if stats.reconciled.Load() != 40 {
		t.Errorf("reconciled = %d, want 40", stats.reconciled.Load())
	}
}
