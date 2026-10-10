package mcp

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

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
