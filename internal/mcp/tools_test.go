package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/qdrant/go-client/qdrant"

	"github.com/gorizond/vcs-rag/internal/graph"
	"github.com/gorizond/vcs-rag/internal/store"
)

// mockSearcher — a mock store.Searcher: records the filter/topK.
type mockSearcher struct {
	gotFilter store.Filter
	gotTopK   int
	calls     int
	points    []store.ScoredPoint
	err       error
}

func (m *mockSearcher) Search(_ context.Context, _ []float32, f store.Filter, topK int) ([]store.ScoredPoint, error) {
	m.calls++
	m.gotFilter = f
	m.gotTopK = topK
	if m.err != nil {
		return nil, m.err
	}
	return m.points, nil
}

// mockEmbedder — a mock embedder: a vector per text.
type mockEmbedder struct {
	err   error
	calls int
}

func (m *mockEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	m.calls++
	if m.err != nil {
		return nil, m.err
	}
	vecs := make([][]float32, len(texts))
	for i := range texts {
		vecs[i] = []float32{1, 2, 3}
	}
	return vecs, nil
}

func TestSearchCodeFilterAndResponse(t *testing.T) {
	ms := &mockSearcher{points: []store.ScoredPoint{{
		ID: "p1", Score: 0.87,
		Payload: map[string]any{
			"repo": "fixture", "group_org": "g", "branch": "main",
			"file_path": "main.go", "commit_sha": "abc", "text": "func main() {}",
		},
	}}}
	tl := &tools{embedder: &mockEmbedder{}, searcher: ms}

	_, out, err := tl.searchCode(context.Background(), nil, searchCodeInput{Query: "main", Repo: "fixture"})
	if err != nil {
		t.Fatalf("searchCode: %v", err)
	}
	if ms.gotFilter.Type != "code" || ms.gotFilter.Repo != "fixture" {
		t.Errorf("filter = %+v, expected type=code repo=fixture", ms.gotFilter)
	}
	if ms.gotTopK != defaultTopK {
		t.Errorf("top_k = %d, expected default %d", ms.gotTopK, defaultTopK)
	}
	resp, ok := out.(resultsResponse[codeHit])
	if !ok {
		t.Fatalf("out = %T, expected resultsResponse[codeHit]", out)
	}
	hits := resp.Results
	if len(hits) != 1 || hits[0].FilePath != "main.go" || hits[0].Score != 0.87 {
		t.Errorf("hits = %+v", hits)
	}
	js := marshalJSON(out)
	// T148 negative control: the structured payload is a record, not an array.
	if strings.HasPrefix(strings.TrimSpace(js), "[") {
		t.Errorf("structured output is an array: %s", js)
	}
	for _, want := range []string{`"results":[`, `"repo":"fixture"`, `"file_path":"main.go"`, `"commit_sha":"abc"`, `"score":0.87`, `"snippet":"func main() {}"`} {
		if !strings.Contains(js, want) {
			t.Errorf("JSON does not contain %s: %s", want, js)
		}
	}
}

func TestTopKClamping(t *testing.T) {
	for _, tc := range []struct{ in, want int }{{0, 5}, {3, 3}, {20, 20}, {50, 20}} {
		ms := &mockSearcher{}
		tl := &tools{embedder: &mockEmbedder{}, searcher: ms}
		_, _, err := tl.searchCode(context.Background(), nil, searchCodeInput{Query: "x", TopK: tc.in})
		if err != nil {
			t.Fatalf("searchCode(top_k=%d): %v", tc.in, err)
		}
		if ms.gotTopK != tc.want {
			t.Errorf("top_k %d → %d, expected %d", tc.in, ms.gotTopK, tc.want)
		}
		if ms.gotFilter.Type != "code" {
			t.Errorf("filter type = %q, expected code", ms.gotFilter.Type)
		}
	}
}

func TestSnippetLimit(t *testing.T) {
	// a multi-byte UTF-8 fixture (2 bytes per rune) — the snippet limit is rune-based.
	long := strings.Repeat("é", 1000)
	ms := &mockSearcher{points: []store.ScoredPoint{{Payload: map[string]any{"text": long}}}}
	tl := &tools{embedder: &mockEmbedder{}, searcher: ms}
	_, out, err := tl.searchCode(context.Background(), nil, searchCodeInput{Query: "x"})
	if err != nil {
		t.Fatal(err)
	}
	hit := out.(resultsResponse[codeHit]).Results[0]
	if n := len([]rune(hit.Snippet)); n > maxSnippet {
		t.Errorf("snippet %d characters > %d", n, maxSnippet)
	}
	if !strings.HasSuffix(hit.Snippet, "…") {
		t.Error("snippet is not truncated with an ellipsis")
	}
	if snip := snippet("short text"); snip != "short text" {
		t.Errorf("short text changed: %q", snip)
	}
}

func TestToolErrors(t *testing.T) {
	tests := []struct {
		name      string
		embedErr  error
		searchErr error
		wantCode  string
	}{
		{name: "qdrant unavailable", searchErr: errors.New("connection refused"), wantCode: codeSearchFailed},
		{name: "embeddings unavailable", embedErr: errors.New("ollama: dial tcp: refused"), wantCode: codeEmbedFailed},
		{name: "timeout", searchErr: context.DeadlineExceeded, wantCode: codeTimeout},
		{name: "canceled", searchErr: context.Canceled, wantCode: codeCanceled},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tl := &tools{embedder: &mockEmbedder{err: tc.embedErr}, searcher: &mockSearcher{err: tc.searchErr}}
			_, out, err := tl.searchCode(context.Background(), nil, searchCodeInput{Query: "x"})
			if err != nil {
				t.Fatalf("handler returned protocol error %v, expected a structured response", err)
			}
			te, ok := out.(toolError)
			if !ok {
				t.Fatalf("out = %T, expected toolError", out)
			}
			if te.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", te.Code, tc.wantCode)
			}
			if te.Message == "" {
				t.Error("message is empty")
			}
			if !strings.Contains(marshalJSON(out), `"code":"`+tc.wantCode+`"`) {
				t.Errorf("JSON = %s", marshalJSON(out))
			}
		})
	}
}

func TestSearchPRsIssuesCIFilters(t *testing.T) {
	prSearcher := &mockSearcher{points: []store.ScoredPoint{{
		Score: 0.5,
		Payload: map[string]any{
			"vcs_type": "gitlab", "repo": "fixture", "pr_id": int64(42),
			"source_branch": "feat", "target_branch": "main", "state": "opened", "text": "PR text",
		},
	}}}
	tl := &tools{embedder: &mockEmbedder{}, searcher: prSearcher}
	_, out, err := tl.searchPRs(context.Background(), nil, metadataQueryInput{Query: "x", State: "opened"})
	if err != nil {
		t.Fatal(err)
	}
	if prSearcher.gotFilter.Type != "pull_request" || prSearcher.gotFilter.State != "opened" {
		t.Errorf("PR filter = %+v", prSearcher.gotFilter)
	}
	pr := out.(resultsResponse[prHit]).Results[0]
	if pr.PRID != 42 || pr.SourceBranch != "feat" || pr.State != "opened" {
		t.Errorf("prHit = %+v", pr)
	}
	if js := marshalJSON(out); !strings.Contains(js, `"pr_id":42`) {
		t.Errorf("JSON PR = %s", js)
	}

	issueSearcher := &mockSearcher{points: []store.ScoredPoint{{
		Score: 0.4,
		Payload: map[string]any{
			"vcs_type": "github", "repo": "fixture", "issue_id": int64(7), "state": "closed", "text": "issue text",
		},
	}}}
	tl = &tools{embedder: &mockEmbedder{}, searcher: issueSearcher}
	_, out, err = tl.searchIssues(context.Background(), nil, metadataQueryInput{Query: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if issueSearcher.gotFilter.Type != "issue" {
		t.Errorf("issues filter = %+v", issueSearcher.gotFilter)
	}
	if issue := out.(resultsResponse[issueHit]).Results[0]; issue.IssueID != 7 || issue.State != "closed" {
		t.Errorf("issueHit = %+v", issue)
	}

	ciSearcher := &mockSearcher{points: []store.ScoredPoint{{
		Score: 0.3,
		Payload: map[string]any{
			"vcs_type": "gitlab", "repo": "fixture", "branch": "main", "commit_sha": "abc",
			"pipeline_id": "100", "job_name": "build", "text": "job log",
		},
	}}}
	tl = &tools{embedder: &mockEmbedder{}, searcher: ciSearcher}
	_, out, err = tl.searchCIFailures(context.Background(), nil, ciFailuresInput{Query: "x", Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if ciSearcher.gotFilter.Type != "ci_failure" || ciSearcher.gotFilter.Branch != "main" {
		t.Errorf("CI filter = %+v", ciSearcher.gotFilter)
	}
	if ci := out.(resultsResponse[ciHit]).Results[0]; ci.PipelineID != "100" || ci.JobName != "build" {
		t.Errorf("ciHit = %+v", ci)
	}
}

// T031: the optional state/label/author/milestone filters reach the store
// filter of search_issues and search_pull_requests; absent fields stay empty.
func TestMetadataQueryFilters(t *testing.T) {
	in := metadataQueryInput{Query: "x", State: "open", Label: "bug", Author: "alice", Milestone: "v1"}

	ms := &mockSearcher{}
	tl := &tools{embedder: &mockEmbedder{}, searcher: ms}
	if _, _, err := tl.searchIssues(context.Background(), nil, in); err != nil {
		t.Fatal(err)
	}
	f := ms.gotFilter
	if f.Type != "issue" || f.State != "open" || f.Author != "alice" || f.Milestone != "v1" ||
		len(f.Labels) != 1 || f.Labels[0] != "bug" {
		t.Errorf("searchIssues filter = %+v", f)
	}

	ms = &mockSearcher{}
	tl.searcher = ms
	if _, _, err := tl.searchPRs(context.Background(), nil, in); err != nil {
		t.Fatal(err)
	}
	if f := ms.gotFilter; f.Type != "pull_request" || f.State != "open" ||
		f.Author != "alice" || f.Milestone != "v1" || len(f.Labels) != 1 {
		t.Errorf("searchPRs filter = %+v", f)
	}

	// absent filters add nothing (backward compatible).
	ms = &mockSearcher{}
	tl.searcher = ms
	if _, _, err := tl.searchIssues(context.Background(), nil, metadataQueryInput{Query: "x"}); err != nil {
		t.Fatal(err)
	}
	if f := ms.gotFilter; f.State != "" || f.Author != "" || f.Milestone != "" || f.Labels != nil {
		t.Errorf("absent filters leaked: %+v", f)
	}
}

// T148: an empty result is the object {"results": []}, never an empty array.
func TestEmptyResultIsEmptyResults(t *testing.T) {
	tl := &tools{embedder: &mockEmbedder{}, searcher: &mockSearcher{}}
	_, out, err := tl.searchCode(context.Background(), nil, searchCodeInput{Query: "nothing"})
	if err != nil {
		t.Fatal(err)
	}
	if js := marshalJSON(out); js != `{"results":[]}` {
		t.Errorf("empty result = %s, expected {\"results\":[]}", js)
	}
	if _, ok := out.(resultsResponse[codeHit]); !ok {
		t.Errorf("out = %T, expected the results record", out)
	}
}

// T133-fix/T134: all four search tools share the normalization — a unique
// short form becomes the payload pair (group_org=namespace, repo=short path),
// ambiguity becomes an OR of pairs, unknown stays raw.
func TestNormalizeRepoFilterAllTools(t *testing.T) {
	lister := &fakeRepoLister{repos: mkRepos(
		"gorizond/devops/nats", "gorizond/devops/api", "gorizond/secrets/runner")}
	tl := &tools{embedder: &mockEmbedder{}, repos: newRepoIndex(lister)}

	ms := &mockSearcher{}
	tl.searcher = ms
	if _, _, err := tl.searchCode(context.Background(), nil, searchCodeInput{Query: "x", Repo: "nats"}); err != nil {
		t.Fatal(err)
	}
	if ms.gotFilter.GroupOrg != "gorizond/devops" || ms.gotFilter.Repo != "nats" || len(ms.gotFilter.RepoRefs) != 0 {
		t.Errorf("searchCode filter = %+v", ms.gotFilter)
	}

	ms = &mockSearcher{}
	tl.searcher = ms
	if _, _, err := tl.searchPRs(context.Background(), nil, metadataQueryInput{Query: "x", Repo: "secrets/runner"}); err != nil {
		t.Fatal(err)
	}
	if ms.gotFilter.GroupOrg != "gorizond/secrets" || ms.gotFilter.Repo != "runner" {
		t.Errorf("searchPRs filter = %+v", ms.gotFilter)
	}

	ms = &mockSearcher{}
	tl.searcher = ms
	if _, _, err := tl.searchIssues(context.Background(), nil, metadataQueryInput{Query: "x", Repo: "api"}); err != nil {
		t.Fatal(err)
	}
	if ms.gotFilter.GroupOrg != "gorizond/devops" || ms.gotFilter.Repo != "api" {
		t.Errorf("searchIssues filter = %+v", ms.gotFilter)
	}

	ms = &mockSearcher{}
	tl.searcher = ms
	if _, _, err := tl.searchCIFailures(context.Background(), nil, ciFailuresInput{Query: "x", Repo: "nats"}); err != nil {
		t.Fatal(err)
	}
	if ms.gotFilter.GroupOrg != "gorizond/devops" || ms.gotFilter.Repo != "nats" {
		t.Errorf("searchCIFailures filter = %+v", ms.gotFilter)
	}

	// ambiguity → OR of pairs, the raw value is not used.
	amb := &tools{embedder: &mockEmbedder{},
		repos: newRepoIndex(&fakeRepoLister{repos: mkRepos("a/x/nats", "b/y/nats")})}
	ms = &mockSearcher{}
	amb.searcher = ms
	if _, _, err := amb.searchCode(context.Background(), nil, searchCodeInput{Query: "x", Repo: "nats"}); err != nil {
		t.Fatal(err)
	}
	if ms.gotFilter.Repo != "" || len(ms.gotFilter.RepoRefs) != 2 ||
		ms.gotFilter.RepoRefs[0] != (store.RepoRef{GroupOrg: "a/x", Repo: "nats"}) ||
		ms.gotFilter.RepoRefs[1] != (store.RepoRef{GroupOrg: "b/y", Repo: "nats"}) {
		t.Errorf("ambiguous filter = %+v", ms.gotFilter)
	}

	// the OR list is capped at 16 pairs.
	var many []string
	for i := 0; i < 20; i++ {
		many = append(many, fmt.Sprintf("g%02d/x/dup", i))
	}
	capped := &tools{embedder: &mockEmbedder{}, repos: newRepoIndex(&fakeRepoLister{repos: mkRepos(many...)})}
	ms = &mockSearcher{}
	capped.searcher = ms
	if _, _, err := capped.searchCode(context.Background(), nil, searchCodeInput{Query: "x", Repo: "dup"}); err != nil {
		t.Fatal(err)
	}
	if len(ms.gotFilter.RepoRefs) != repoMaxCandidates ||
		ms.gotFilter.RepoRefs[0] != (store.RepoRef{GroupOrg: "g00/x", Repo: "dup"}) {
		t.Errorf("capped refs = %d %+v", len(ms.gotFilter.RepoRefs), ms.gotFilter.RepoRefs)
	}
}

// T133-fix: the negative control — a full-path input and its short form build
// the same payload filter (both resolved through the index).
func TestNormalizeRepoFilterFullPathSameAsShort(t *testing.T) {
	lister := &fakeRepoLister{repos: mkRepos("example-orgexample-group/gitops/databases")}
	tl := &tools{embedder: &mockEmbedder{}, repos: newRepoIndex(lister)}

	msFull := &mockSearcher{}
	tl.searcher = msFull
	if _, _, err := tl.searchCode(context.Background(), nil, searchCodeInput{
		Query: "x", Repo: "example-orgexample-group/gitops/databases"}); err != nil {
		t.Fatal(err)
	}
	msShort := &mockSearcher{}
	tl.searcher = msShort
	if _, _, err := tl.searchCode(context.Background(), nil, searchCodeInput{Query: "x", Repo: "databases"}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(msFull.gotFilter, msShort.gotFilter) {
		t.Errorf("full path filter = %+v, short form = %+v", msFull.gotFilter, msShort.gotFilter)
	}
	want := store.Filter{GroupOrg: "example-orgexample-group/gitops", Repo: "databases", Type: "code"}
	if !reflect.DeepEqual(msFull.gotFilter, want) {
		t.Errorf("filter = %+v, want the stored pair %+v", msFull.gotFilter, want)
	}
}

// T133-fix: without the index the raw value is passed through as before.
func TestNormalizeRepoFilterNoIndex(t *testing.T) {
	ms := &mockSearcher{}
	tl := &tools{embedder: &mockEmbedder{}, searcher: ms}
	if _, _, err := tl.searchCode(context.Background(), nil, searchCodeInput{Query: "x", Repo: "nats"}); err != nil {
		t.Fatal(err)
	}
	if ms.gotFilter.Repo != "nats" || ms.gotFilter.RepoRefs != nil || ms.gotFilter.GroupOrg != "" {
		t.Errorf("passthrough filter = %+v", ms.gotFilter)
	}
}

// --- T151: index_status ---

// fakeStatusReader serves the worker-status KV bytes.
type fakeStatusReader struct {
	data []byte
	ok   bool
}

func (f *fakeStatusReader) WorkerStatus() ([]byte, bool) { return f.data, f.ok }

// T151: index_status returns the worker status as a record (structuredContent
// object + the same content text), never an array.
func TestIndexStatusTool(t *testing.T) {
	points := uint64(11)
	view := workerStatusView{
		ReposTotal: 3, ReposIndexed: 2, ReposPending: 1,
		PendingSample: []string{"g/c"}, Points: &points,
		LastPass:  &indexPassView{Repos: 3, OK: 2, Errors: 1, IndexedCommits: 7, DurationMS: 42},
		UpdatedAt: "2026-01-01T00:00:00Z",
	}
	data, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	tl := &tools{embedder: &mockEmbedder{}, searcher: &mockSearcher{},
		status: &fakeStatusReader{data: data, ok: true}}

	_, out, err := tl.indexStatus(context.Background(), nil, indexStatusInput{})
	if err != nil {
		t.Fatalf("indexStatus: %v", err)
	}
	resp, ok := out.(indexStatusResponse)
	if !ok || !resp.Available || resp.ReposIndexed != 2 || resp.PendingSample[0] != "g/c" ||
		resp.LastPass == nil || resp.LastPass.IndexedCommits != 7 {
		t.Fatalf("response = %+v (ok=%v)", out, ok)
	}
	js := marshalJSON(out)
	if strings.HasPrefix(strings.TrimSpace(js), "[") {
		t.Errorf("structured output is an array: %s", js)
	}
	for _, want := range []string{`"available":true`, `"repos_total":3`, `"repos_indexed":2`,
		`"pending_sample":["g/c"]`, `"points":11`, `"indexed_commits":7`, `"updated_at":"2026-01-01T00:00:00Z"`} {
		if !strings.Contains(js, want) {
			t.Errorf("JSON does not contain %s: %s", want, js)
		}
	}
}

// T151: unavailable answers are records with available=false and a reason;
// the nil embedded status keeps them flat (no zeroed status fields).
func TestIndexStatusUnavailable(t *testing.T) {
	cases := []struct {
		name   string
		status statusReader
		reason string
	}{
		{"no NATS", nil, "NATS is not configured"},
		{"no data", &fakeStatusReader{}, "no worker status published yet"},
		{"unreadable", &fakeStatusReader{data: []byte("{"), ok: true}, "worker status is unreadable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tl := &tools{embedder: &mockEmbedder{}, searcher: &mockSearcher{}, status: tc.status}
			_, out, err := tl.indexStatus(context.Background(), nil, indexStatusInput{})
			if err != nil {
				t.Fatalf("indexStatus: %v", err)
			}
			resp, ok := out.(indexStatusResponse)
			if !ok || resp.Available || !strings.Contains(resp.Reason, tc.reason) {
				t.Fatalf("out = %+v", out)
			}
			js := marshalJSON(out)
			if strings.HasPrefix(strings.TrimSpace(js), "[") {
				t.Errorf("structured output is an array: %s", js)
			}
			if !strings.Contains(js, `"available":false`) || !strings.Contains(js, `"reason":`) {
				t.Errorf("JSON = %s", js)
			}
			if strings.Contains(js, `"repos_total"`) {
				t.Errorf("unavailable response leaked status fields: %s", js)
			}
		})
	}
}

// fakeGraphReader — a mock mcp.GraphReader (the payload-only graph surface).
type fakeGraphReader struct {
	gotFilter *qdrant.Filter
	filters   []*qdrant.Filter // every call, in order (query_graph asserts the start scan)
	points    []*qdrant.RetrievedPoint
	err       error
}

func (f *fakeGraphReader) ScrollAll(_ context.Context, flt *qdrant.Filter, _ uint32) ([]*qdrant.RetrievedPoint, error) {
	f.gotFilter = flt
	f.filters = append(f.filters, flt)
	if f.err != nil {
		return nil, f.err
	}
	return f.points, nil
}

// T026/SC-002: search_pull_requests(file_path) is the exact mr_file lookup —
// filtered by repo+file_path, MRs grouped with their diff chunks. The query is
// not embedded in this mode.
func TestSearchPRsFilePathLookup(t *testing.T) {
	graph := &fakeGraphReader{points: []*qdrant.RetrievedPoint{
		{Payload: qdrant.NewValueMap(map[string]any{
			"repo": "acme/demo", "vcs_type": "gitlab", "mr_id": 7, "state": "opened",
			"file_path": "a.go", "status": "modified", "patch": "@@ diff @@", "patch_truncated": false,
		})},
		{Payload: qdrant.NewValueMap(map[string]any{
			"repo": "acme/demo", "vcs_type": "gitlab", "mr_id": 9, "state": "merged",
			"file_path": "a.go", "status": "renamed", "patch": "@@ big @@", "patch_truncated": true,
		})},
	}}
	emb := &mockEmbedder{}
	tl := &tools{embedder: emb, searcher: &mockSearcher{}, graph: graph}

	_, out, err := tl.searchPRs(context.Background(), nil, metadataQueryInput{Query: "ignored", Repo: "acme/demo", FilePath: "a.go"})
	if err != nil {
		t.Fatalf("searchPRs(file_path): %v", err)
	}
	if emb.calls != 0 {
		t.Errorf("file_path lookup embedded %d texts, want 0", emb.calls)
	}
	if graph.gotFilter == nil || len(graph.gotFilter.Must) != 3 {
		t.Fatalf("filter = %+v, want kind+repo+file_path", graph.gotFilter)
	}
	resp, ok := out.(resultsResponse[prFileHit])
	if !ok || len(resp.Results) != 2 {
		t.Fatalf("out = %T %+v", out, out)
	}
	if resp.Results[0].PRID != 7 || resp.Results[0].Files[0].Patch != "@@ diff @@" {
		t.Errorf("first MR = %+v", resp.Results[0])
	}
	if resp.Results[1].PRID != 9 || !resp.Results[1].Files[0].PatchTruncated {
		t.Errorf("second MR = %+v", resp.Results[1])
	}
	js := marshalJSON(out)
	for _, want := range []string{`"pr_id":7`, `"file_path":"a.go"`, `"patch":"@@ diff @@"`, `"patch_truncated":true`} {
		if !strings.Contains(js, want) {
			t.Errorf("JSON does not contain %s: %s", want, js)
		}
	}

	// repo is required: without it the lookup cannot be repo-scoped.
	tl2 := &tools{embedder: &mockEmbedder{}, searcher: &mockSearcher{}, graph: graph}
	_, out, err = tl2.searchPRs(context.Background(), nil, metadataQueryInput{Query: "x", FilePath: "a.go"})
	if err != nil {
		t.Fatalf("searchPRs(no repo): %v", err)
	}
	if te, ok := out.(toolError); !ok || te.Code != codeInvalidInput {
		t.Fatalf("no-repo lookup = %T %+v, want invalid_input", out, out)
	}
}

// T037: query_graph compiles the DSL, traverses in one hop and returns the
// flattened points; a non-selective query is refused with the contractual code
// before any scan.
func TestQueryGraphTool(t *testing.T) {
	points := []*qdrant.RetrievedPoint{
		{Id: qdrant.NewID("11111111-1111-5111-8111-111111111111"), Payload: qdrant.NewValueMap(map[string]any{
			"kind": "node", "repo": "acme/demo", "package": "a", "file_path": "a.go", "symbol": "F",
		})},
		{Id: qdrant.NewID("22222222-2222-5222-8222-222222222222"), Payload: qdrant.NewValueMap(map[string]any{
			"kind": "edge", "edge_type": "imports", "repo": "acme/demo", "from_file": "a.go", "to_package": "pkg\x00acme/demo\x00b",
		})},
	}
	graph := &fakeGraphReader{points: points}
	tl := &tools{embedder: &mockEmbedder{}, searcher: &mockSearcher{}, graph: graph}
	// Registration smoke: AddTool panics on a bad derived schema, so building
	// the server once proves the query_graph input schema (nested struct + map
	// fields) is valid.
	registerTools(mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil), tl)

	_, out, err := tl.queryGraph(context.Background(), nil, queryGraphInput{
		Start: queryGraphStart{Entity: "definition", Filters: map[string]any{"repo": "acme/demo", "file_path": "a.go"}},
		Edge:  queryGraphEdge{Type: "imports", Direction: "outgoing"},
		Hops:  1,
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("queryGraph: %v", err)
	}
	resp, ok := out.(queryGraphResponse)
	if !ok {
		t.Fatalf("out = %T, want queryGraphResponse", out)
	}
	if len(resp.Results) != 2 || resp.Truncated {
		t.Fatalf("results=%d truncated=%v, want 2/false", len(resp.Results), resp.Truncated)
	}
	if resp.Results[0]["id"] != "11111111-1111-5111-8111-111111111111" || resp.Results[0]["kind"] != "node" {
		t.Errorf("first hit = %+v", resp.Results[0])
	}
	// The compiled start filter (kind+node_type+repo+file_path) is the first scan.
	if len(graph.filters) == 0 || len(graph.filters[0].Must) != 4 {
		t.Fatalf("start filter not passed to the store: %+v", graph.filters)
	}
	js := marshalJSON(out)
	for _, want := range []string{`"truncated":false`, `"kind":"edge"`, `"from_file":"a.go"`} {
		if !strings.Contains(js, want) {
			t.Errorf("JSON does not contain %s: %s", want, js)
		}
	}

	// Non-selective: refused with code non_selective_query and no scan at all.
	graph2 := &fakeGraphReader{points: points}
	tl2 := &tools{embedder: &mockEmbedder{}, searcher: &mockSearcher{}, graph: graph2}
	_, out, err = tl2.queryGraph(context.Background(), nil, queryGraphInput{
		Start: queryGraphStart{Entity: "definition", Filters: map[string]any{"branch": "main"}},
	})
	if err != nil {
		t.Fatalf("queryGraph(non-selective): %v", err)
	}
	if te, ok := out.(toolError); !ok || te.Code != codeNonSelective {
		t.Fatalf("non-selective = %T %+v, want code %q", out, out, codeNonSelective)
	}
	if len(graph2.filters) != 0 {
		t.Error("non-selective query reached the store: it must be rejected before any scan")
	}
}

// --- T053: unit tests of the remaining graph tool handlers ---

// scriptedGraphReader — a fakeGraphReader with one scripted response per call
// (the last response repeats); lets a test feed several logical scans in order.
type scriptedGraphReader struct {
	fakeGraphReader
	responses [][]*qdrant.RetrievedPoint
}

func (s *scriptedGraphReader) ScrollAll(_ context.Context, flt *qdrant.Filter, _ uint32) ([]*qdrant.RetrievedPoint, error) {
	s.gotFilter = flt
	s.filters = append(s.filters, flt)
	if s.err != nil {
		return nil, s.err
	}
	if len(s.responses) == 0 {
		return nil, nil
	}
	out := s.responses[0]
	if len(s.responses) > 1 {
		s.responses = s.responses[1:]
	}
	return out, nil
}

// conditionField — the payload field a compiled condition matches on.
func conditionField(c *qdrant.Condition) string {
	if fc := c.GetField(); fc != nil {
		return fc.GetKey()
	}
	return ""
}

// hasKeyword reports whether the filter's must chain has a direct
// field=value MatchAny condition.
func hasKeyword(f *qdrant.Filter, field, value string) bool {
	for _, c := range f.GetMust() {
		fc := c.GetField()
		if fc == nil || fc.GetKey() != field {
			continue
		}
		for _, v := range fc.GetMatch().GetKeywords().GetStrings() {
			if v == value {
				return true
			}
		}
	}
	return false
}

// nestedKeyword reports whether a nested (OR) condition of the filter's must
// chain matches field=value in its should list.
func nestedKeyword(f *qdrant.Filter, field, value string) bool {
	for _, c := range f.GetMust() {
		nf := c.GetFilter()
		if nf == nil {
			continue
		}
		for _, sc := range nf.GetShould() {
			fc := sc.GetField()
			if fc == nil || fc.GetKey() != field {
				continue
			}
			for _, v := range fc.GetMatch().GetKeywords().GetStrings() {
				if v == value {
					return true
				}
			}
		}
	}
	return false
}

// defNode — one definition node payload (the graph store's canonical shape).
func defNode(repo, file, symbol string) *qdrant.RetrievedPoint {
	return &qdrant.RetrievedPoint{Payload: qdrant.NewValueMap(map[string]any{
		"kind": "node", "node_type": "definition", "repo": repo, "file_path": file,
		"symbol": symbol, "symbol_kind": "func", "package": "a", "signature": "func " + symbol + "()",
		"commit_sha": "abc",
	})}
}

// impEdge — one edge_type=imports payload between repo-scoped packages.
func impEdge(repo, fromFile, fromPkg, toPkg string) *qdrant.RetrievedPoint {
	return &qdrant.RetrievedPoint{Payload: qdrant.NewValueMap(map[string]any{
		"kind": "edge", "edge_type": "imports", "repo": repo, "from_file": fromFile,
		"from_package": graph.PkgKey(repo, fromPkg), "to_package": graph.PkgKey(repo, toPkg),
		"external": false,
	})}
}

// T053: search_definitions — name mode is a filter (no embedding), the envelope
// is a record with results+coverage, invalid inputs are refused before a scan.
func TestSearchDefinitionsTool(t *testing.T) {
	graph := &fakeGraphReader{points: []*qdrant.RetrievedPoint{defNode("acme/demo", "a.go", "F")}}
	emb := &mockEmbedder{}
	tl := &tools{embedder: emb, searcher: &mockSearcher{}, graph: graph}

	_, out, err := tl.searchDefinitions(context.Background(), nil, searchDefinitionsInput{
		Query: "F", Repo: "acme/demo", Mode: "name",
	})
	if err != nil {
		t.Fatalf("searchDefinitions: %v", err)
	}
	if emb.calls != 0 {
		t.Errorf("name mode embedded %d texts, want 0", emb.calls)
	}
	resp, ok := out.(definitionsResponse)
	if !ok || len(resp.Results) != 1 {
		t.Fatalf("out = %T %+v, want one definition", out, out)
	}
	if resp.Results[0].Symbol != "F" || resp.Results[0].CommitSHA != "abc" {
		t.Errorf("hit = %+v", resp.Results[0])
	}
	// The name filter: kind+node_type+repo in must, symbol/symbol_text in should
	// (filters[0] is the definition scan; a later scan reads the coverage marker).
	nameFilter := graph.filters[0]
	if len(nameFilter.Must) != 3 || nameFilter.MinShould == nil ||
		len(nameFilter.Should) != 2 {
		t.Fatalf("name filter = %+v, want kind+node_type+repo with a symbol should-chain", nameFilter)
	}
	if len(resp.Coverage) != 1 || resp.Coverage[0].Repo != "acme/demo" {
		t.Errorf("coverage = %+v", resp.Coverage)
	}
	js := marshalJSON(out)
	if strings.HasPrefix(strings.TrimSpace(js), "[") {
		t.Errorf("structured output is an array: %s", js)
	}
	for _, want := range []string{`"results":[`, `"coverage":[`, `"symbol":"F"`, `"commit_sha":"abc"`} {
		if !strings.Contains(js, want) {
			t.Errorf("JSON does not contain %s: %s", want, js)
		}
	}

	// Input parsing: empty query and an unknown mode are invalid_input.
	for _, in := range []searchDefinitionsInput{{Query: "  "}, {Query: "F", Mode: "fuzzy"}} {
		_, o, e := tl.searchDefinitions(context.Background(), nil, in)
		if e != nil {
			t.Fatalf("searchDefinitions(%+v): %v", in, e)
		}
		if te, ok := o.(toolError); !ok || te.Code != codeInvalidInput {
			t.Errorf("input %+v = %T %+v, want %s", in, o, o, codeInvalidInput)
		}
	}

	// Without a graph store every graph tool reports qdrant_unavailable.
	noGraph := &tools{embedder: &mockEmbedder{}, searcher: &mockSearcher{}}
	_, out, err = noGraph.searchDefinitions(context.Background(), nil, searchDefinitionsInput{Query: "F"})
	if err != nil {
		t.Fatalf("searchDefinitions(no graph): %v", err)
	}
	if te, ok := out.(toolError); !ok || te.Code != codeSearchFailed {
		t.Errorf("no-graph = %T %+v, want %s", out, out, codeSearchFailed)
	}
}

// T053: search_definitions auto mode falls back to the semantic join: the name
// scan finds nothing, the vector search hits a file, the join adds the score.
func TestSearchDefinitionsSemanticFallback(t *testing.T) {
	def := defNode("acme/demo", "a.go", "F")
	seq := &scriptedGraphReader{responses: [][]*qdrant.RetrievedPoint{
		{},    // name scan: nothing
		{def}, // definition join on (repo, file_path)
		{def}, // coverage marker scan
	}}
	emb := &mockEmbedder{}
	ms := &mockSearcher{points: []store.ScoredPoint{{Score: 0.9, Payload: map[string]any{
		"group_org": "acme", "repo": "demo", "file_path": "a.go", "text": "code",
	}}}}
	tl := &tools{embedder: emb, searcher: ms, graph: seq}

	_, out, err := tl.searchDefinitions(context.Background(), nil, searchDefinitionsInput{
		Query: "explain F", Repo: "acme/demo", // mode auto
	})
	if err != nil {
		t.Fatalf("searchDefinitions(auto): %v", err)
	}
	if emb.calls != 1 || ms.calls != 1 {
		t.Errorf("fallback embed/search calls = %d/%d, want 1/1", emb.calls, ms.calls)
	}
	resp, ok := out.(definitionsResponse)
	if !ok || len(resp.Results) != 1 || resp.Results[0].Score != 0.9 || resp.Results[0].Symbol != "F" {
		t.Fatalf("out = %T %+v, want the joined definition with score 0.9", out, out)
	}
	// The join scan is keyed on the vector hit: kind+node_type+repo+file_path.
	if len(seq.filters) < 2 || len(seq.filters[1].Must) != 4 {
		t.Fatalf("join filter not passed to the store: %+v", seq.filters)
	}
}

// T053: get_imports/get_dependents — the outgoing/incoming filters, the
// file→package resolution of get_dependents, and the validation branches.
func TestGetImportsDependentsTool(t *testing.T) {
	const repo = "acme/demo"
	edge := impEdge(repo, "a.go", "a", "b")
	wantB := graph.PkgKey(repo, "b")

	gr := &fakeGraphReader{points: []*qdrant.RetrievedPoint{edge}}
	tl := &tools{embedder: &mockEmbedder{}, searcher: &mockSearcher{}, graph: gr}

	// get_imports: outgoing edges of the file (edge_type+repo+from_file).
	_, out, err := tl.getImports(context.Background(), nil, graphEdgesInput{Repo: repo, FilePath: "a.go"})
	if err != nil {
		t.Fatalf("getImports: %v", err)
	}
	resp, ok := out.(importsResponse)
	if !ok || len(resp.Results) != 1 || resp.Results[0].FromFile != "a.go" ||
		resp.Results[0].ToPackage != wantB {
		t.Fatalf("out = %T %+v", out, out)
	}
	if len(gr.filters) == 0 || len(gr.filters[0].Must) != 3 || conditionField(gr.filters[0].Must[2]) != "from_file" {
		t.Fatalf("outgoing filter = %+v, want edge_type+repo+from_file", gr.filters)
	}
	js := marshalJSON(out)
	if !strings.Contains(js, `"from_file":"a.go"`) || !strings.Contains(js, `"results":[`) {
		t.Errorf("JSON = %s", js)
	}

	// get_imports by package: any spelling reduces to the bare package name —
	// from_package stores the source file's bare package.
	gr = &fakeGraphReader{points: []*qdrant.RetrievedPoint{edge}}
	tl.graph = gr
	if _, _, err := tl.getImports(context.Background(), nil, graphEdgesInput{Repo: repo, Package: "github.com/x/y/pkg/a"}); err != nil {
		t.Fatalf("getImports(package path): %v", err)
	}
	if len(gr.filters) == 0 || !hasKeyword(gr.filters[0], "from_package", graph.PkgKey(repo, "a")) {
		t.Errorf("outgoing package filter = %+v, want from_package=%q", gr.filters, graph.PkgKey(repo, "a"))
	}

	// get_dependents by bare package name: the edge key form is the bare
	// to_package_name (definition nodes carry `package` = "b"), not the
	// full-path to_package key.
	gr = &fakeGraphReader{points: []*qdrant.RetrievedPoint{edge}}
	tl.graph = gr
	_, out, err = tl.getDependents(context.Background(), nil, graphEdgesInput{Repo: repo, Package: "b"})
	if err != nil {
		t.Fatalf("getDependents(package): %v", err)
	}
	if _, ok := out.(importsResponse); !ok || len(gr.filters) == 0 || len(gr.filters[0].Must) != 3 {
		t.Fatalf("out = %T filters = %+v", out, gr.filters)
	}
	if !nestedKeyword(gr.filters[0], "to_package_name", "b") {
		t.Errorf("bare package filter misses to_package_name: %+v", gr.filters[0])
	}

	// get_dependents by full import path: both key forms are matched — the
	// repo-namespaced to_package key and the bare last segment.
	full := "github.com/x/y/pkg/b"
	fullKey := graph.PkgKey(repo, full)
	gr = &fakeGraphReader{points: []*qdrant.RetrievedPoint{edge}}
	tl.graph = gr
	_, out, err = tl.getDependents(context.Background(), nil, graphEdgesInput{Repo: repo, Package: full})
	if err != nil {
		t.Fatalf("getDependents(full path): %v", err)
	}
	if _, ok := out.(importsResponse); !ok || len(gr.filters) == 0 {
		t.Fatalf("out = %T filters = %+v", out, gr.filters)
	}
	if !nestedKeyword(gr.filters[0], "to_package", fullKey) || !nestedKeyword(gr.filters[0], "to_package_name", "b") {
		t.Errorf("full-path filter = %+v, want to_package(PkgKey) OR to_package_name(b)", gr.filters[0])
	}

	// get_dependents by file: the file is resolved to its bare package(s).
	seq := &scriptedGraphReader{responses: [][]*qdrant.RetrievedPoint{
		{defNode(repo, "a.go", "F")}, // definitions → package "a"
		{edge},                       // importers of to_package_name=a
		{defNode(repo, "a.go", "F")}, // coverage marker scan
	}}
	tl.graph = seq
	_, out, err = tl.getDependents(context.Background(), nil, graphEdgesInput{Repo: repo, FilePath: "a.go"})
	if err != nil {
		t.Fatalf("getDependents(file): %v", err)
	}
	if r, ok := out.(importsResponse); !ok || len(r.Results) != 1 || len(seq.filters) < 2 {
		t.Fatalf("out = %T %+v filters = %d", out, out, len(seq.filters))
	}
	if !hasKeyword(seq.filters[1], "to_package_name", "a") {
		t.Errorf("resolved file filter = %+v, want to_package_name=a", seq.filters[1])
	}

	// A file without indexed definitions resolves to no package: an honest empty
	// result, no edge scan (definitions scan + coverage scan only).
	empty := &scriptedGraphReader{responses: [][]*qdrant.RetrievedPoint{{}, {}}}
	tl.graph = empty
	_, out, err = tl.getDependents(context.Background(), nil, graphEdgesInput{Repo: repo, FilePath: "gone.go"})
	if err != nil {
		t.Fatalf("getDependents(unresolved): %v", err)
	}
	if r, ok := out.(importsResponse); !ok || len(r.Results) != 0 {
		t.Fatalf("unresolved file out = %T %+v, want an empty result", out, out)
	}
	if len(empty.filters) != 2 {
		t.Errorf("unresolved lookup scrolled %d times, want definitions+coverage only", len(empty.filters))
	}

	// Input parsing: repo, exactly-one-of and direction validation.
	for _, in := range []graphEdgesInput{
		{FilePath: "a.go"}, // no repo
		{Repo: repo},       // neither file_path nor package
		{Repo: repo, FilePath: "a.go", Package: "a"},          // both
		{Repo: repo, FilePath: "a.go", Direction: "sideways"}, // bad direction
	} {
		_, o, e := tl.getImports(context.Background(), nil, in)
		if e != nil {
			t.Fatalf("getImports(%+v): %v", in, e)
		}
		if te, ok := o.(toolError); !ok || te.Code != codeInvalidInput {
			t.Errorf("input %+v = %T %+v, want %s", in, o, o, codeInvalidInput)
		}
	}
}

// T053: graph_schema — the self-description record (node/edge/entity types, DSL
// version, coverage markers); a nil graph still answers without coverage.
func TestGraphSchemaTool(t *testing.T) {
	mark := &qdrant.RetrievedPoint{Payload: qdrant.NewValueMap(map[string]any{
		"kind": "coverage", "repo": "acme/demo", "indexed_commit": "abc123",
		"generation": int64(3), "indexed_at": "2026-01-01T00:00:00Z",
		"languages_with_resolvers": []any{"go"},
	})}
	graph := &fakeGraphReader{points: []*qdrant.RetrievedPoint{mark}}
	tl := &tools{embedder: &mockEmbedder{}, searcher: &mockSearcher{}, graph: graph, graphSchemaVer: "7"}

	_, out, err := tl.graphSchema(context.Background(), nil, graphSchemaInput{Repo: "acme/demo"})
	if err != nil {
		t.Fatalf("graphSchema: %v", err)
	}
	resp, ok := out.(graphSchemaResponse)
	if !ok {
		t.Fatalf("out = %T", out)
	}
	if !hasName(resp.NodeTypes, func(n nodeTypeSpec) string { return n.Name }, "definition") ||
		!hasName(resp.EdgeTypes, func(n edgeTypeSpec) string { return n.Name }, "imports") ||
		!hasName(resp.EdgeTypes, func(n edgeTypeSpec) string { return n.Name }, "defines") {
		t.Errorf("schema types = %+v / %+v", resp.NodeTypes, resp.EdgeTypes)
	}
	for _, entity := range []string{"mr_file", "ci", "vuln"} {
		if !hasName(resp.EntityTypes, func(n entityTypeSpec) string { return n.Name }, entity) {
			t.Errorf("entity_types lacks %q: %+v", entity, resp.EntityTypes)
		}
	}
	if resp.QueryDSL.Version != "7" || len(resp.QueryDSL.Operators) == 0 {
		t.Errorf("query_dsl = %+v", resp.QueryDSL)
	}
	if len(resp.CoveragePerRepo) != 1 || resp.CoveragePerRepo[0].IndexedCommit != "abc123" ||
		resp.CoveragePerRepo[0].Generation != 3 || resp.CoveragePerRepo[0].LanguagesWithResolvers[0] != "go" {
		t.Errorf("coverage_per_repo = %+v", resp.CoveragePerRepo)
	}
	js := marshalJSON(out)
	for _, want := range []string{`"node_types":[`, `"entity_types":[`, `"query_dsl":{`, `"coverage_per_repo":[`, `"indexed_commit":"abc123"`} {
		if !strings.Contains(js, want) {
			t.Errorf("JSON does not contain %s: %s", want, js)
		}
	}

	// The default DSL version and the no-store answer.
	tl = &tools{embedder: &mockEmbedder{}, searcher: &mockSearcher{}}
	_, out, err = tl.graphSchema(context.Background(), nil, graphSchemaInput{})
	if err != nil {
		t.Fatalf("graphSchema(no graph): %v", err)
	}
	if r, ok := out.(graphSchemaResponse); !ok || r.QueryDSL.Version != "1" || len(r.CoveragePerRepo) != 0 {
		t.Errorf("no-graph schema = %T %+v", out, out)
	}
	if js := marshalJSON(out); !strings.Contains(js, `"coverage_per_repo":[]`) {
		t.Errorf("no-graph JSON = %s", js)
	}
}

// hasName — the schema type-list membership check (one spec list at a time).
func hasName[T any](names []T, getName func(T) string, want string) bool {
	for _, n := range names {
		if getName(n) == want {
			return true
		}
	}
	return false
}

// T053: query_graph input parsing — the filter operator forms reach the store as
// compiled conditions; unsupported forms and unknown entities/edges are
// invalid_input; a non-selective ci query is refused with non_selective_query.
func TestQueryGraphFilterForms(t *testing.T) {
	graph := &fakeGraphReader{}
	tl := &tools{embedder: &mockEmbedder{}, searcher: &mockSearcher{}, graph: graph}

	// A range form on top of the selective repo, entity defaulting to definition.
	_, out, err := tl.queryGraph(context.Background(), nil, queryGraphInput{
		Start: queryGraphStart{Filters: map[string]any{"repo": "acme/demo", "duration_ms": map[string]any{"gte": 100.0}}},
		Edge:  queryGraphEdge{Type: "imports", Direction: "incoming"},
		Hops:  2,
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("queryGraph: %v", err)
	}
	resp, ok := out.(queryGraphResponse)
	if !ok || len(resp.Results) != 0 || resp.Truncated {
		t.Fatalf("out = %T %+v, want an empty non-truncated answer", out, out)
	}
	// must = kind + node_type + duration_ms(range) + repo, sorted by field.
	if len(graph.filters) == 0 || len(graph.filters[0].Must) != 4 {
		t.Fatalf("start filter = %+v, want kind+node_type+range+repo", graph.filters)
	}
	if c := graph.filters[0].Must[2]; conditionField(c) != "duration_ms" || c.GetField().GetRange() == nil {
		t.Errorf("range condition = %+v", c)
	}
	js := marshalJSON(out)
	for _, want := range []string{`"results":[]`, `"coverage":[`, `"truncated":false`} {
		if !strings.Contains(js, want) {
			t.Errorf("JSON does not contain %s: %s", want, js)
		}
	}

	// Unsupported filter forms are invalid_input, never silently dropped.
	for name, filters := range map[string]map[string]any{
		"mixed list":       {"repo": "acme/demo", "node_type": []any{"definition", 1.0}},
		"bad range bound":  {"repo": "acme/demo", "duration_ms": map[string]any{"gte": []any{1.0}}},
		"unknown range op": {"repo": "acme/demo", "duration_ms": map[string]any{"eq": 1.0}},
	} {
		_, o, e := tl.queryGraph(context.Background(), nil, queryGraphInput{
			Start: queryGraphStart{Entity: "definition", Filters: filters},
		})
		if e != nil {
			t.Fatalf("%s: %v", name, e)
		}
		if te, ok := o.(toolError); !ok || te.Code != codeInvalidInput {
			t.Errorf("%s = %T %+v, want %s", name, o, o, codeInvalidInput)
		}
	}
	for name, in := range map[string]queryGraphInput{
		"bad entity":    {Start: queryGraphStart{Entity: "bogus", Filters: map[string]any{"repo": "r"}}},
		"bad edge":      {Start: queryGraphStart{Entity: "ci", Filters: map[string]any{"repo": "r"}}, Edge: queryGraphEdge{Type: "calls"}},
		"bad direction": {Start: queryGraphStart{Entity: "ci", Filters: map[string]any{"repo": "r"}}, Edge: queryGraphEdge{Direction: "sideways"}},
	} {
		_, o, e := tl.queryGraph(context.Background(), nil, in)
		if e != nil {
			t.Fatalf("%s: %v", name, e)
		}
		if te, ok := o.(toolError); !ok || te.Code != codeInvalidInput {
			t.Errorf("%s = %T %+v, want %s", name, o, o, codeInvalidInput)
		}
	}

	// The ci entity without a selective predicate: non_selective_query, no scan.
	scans := len(graph.filters)
	_, out, err = tl.queryGraph(context.Background(), nil, queryGraphInput{
		Start: queryGraphStart{Entity: "ci", Filters: map[string]any{"status": "failed"}},
	})
	if err != nil {
		t.Fatalf("queryGraph(non-selective ci): %v", err)
	}
	if te, ok := out.(toolError); !ok || te.Code != codeNonSelective {
		t.Fatalf("non-selective ci = %T %+v, want %s", out, out, codeNonSelective)
	}
	if len(graph.filters) != scans {
		t.Error("non-selective ci query reached the store: it must be rejected before any scan")
	}
}

// T053: registration smoke — all graph tools register without a schema panic and
// are visible to a client.
func TestGraphToolsRegistered(t *testing.T) {
	ctx := context.Background()
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	tl := &tools{embedder: &mockEmbedder{}, searcher: &mockSearcher{}, graph: &fakeGraphReader{}}
	registerTools(server, tl) // panics on an invalid derived input schema

	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer cs.Close()

	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	got := make(map[string]bool, len(res.Tools))
	for _, tool := range res.Tools {
		got[tool.Name] = true
	}
	for _, name := range []string{"search_definitions", "get_imports", "get_dependents", "graph_schema", "query_graph"} {
		if !got[name] {
			t.Errorf("graph tool %q is not registered", name)
		}
	}
}
