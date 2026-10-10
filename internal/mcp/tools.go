package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gorizond/vcs-rag/internal/store"
)

// Searcher — a narrow store interface for the tools (mock in tests).
type Searcher interface {
	Search(ctx context.Context, vector []float32, f store.Filter, topK int) ([]store.ScoredPoint, error)
}

// Embedder — a narrow embeddings interface for the tools (mock in tests).
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

const (
	toolTimeout = 60 * time.Second // per-call limit (timeouts → code timeout)
	defaultTopK = 5
	maxTopK     = 20 // cap from contracts/mcp-tools.md
	maxSnippet  = 400
)

// tools — MCP tool implementations on top of narrow interfaces.
type tools struct {
	embedder  Embedder
	searcher  Searcher
	refresher refresher    // nil when NATS_URL is empty (refresh_repo is off)
	inflight  *flightGroup // in-flight share of refresh_repo (T113)
	repos     *repoIndex   // nil — the repo filter is passed through as before (T132)
	auto      *autoClient  // nil when NATS_URL is empty — auto-refresh is off (T134)
}

// newTools assembles the tool set; a nil refresher disables refresh_repo.
func newTools(embedder Embedder, searcher Searcher, refresher refresher) *tools {
	t := &tools{embedder: embedder, searcher: searcher, refresher: refresher}
	if refresher != nil {
		t.inflight = newFlightGroup()
	}
	return t
}

// --- inputs (contracts/mcp-tools.md) ---

type searchCodeInput struct {
	Query    string `json:"query" jsonschema:"search query text"`
	VCSType  string `json:"vcs_type,omitempty" jsonschema:"filter: gitlab | github"`
	GroupOrg string `json:"group_org,omitempty" jsonschema:"GitLab group or GitHub organization"`
	Repo     string `json:"repo,omitempty" jsonschema:"repository path, or a unique short form (e.g. \"nats\", \"secrets/runner\")"`
	Branch   string `json:"branch,omitempty" jsonschema:"branch"`
	TopK     int    `json:"top_k,omitempty" jsonschema:"how many results to return (default 5, maximum 20)"`
}

type metadataQueryInput struct {
	Query    string `json:"query" jsonschema:"search query text"`
	Repo     string `json:"repo,omitempty" jsonschema:"repository path, or a unique short form (e.g. \"nats\", \"secrets/runner\")"`
	GroupOrg string `json:"group_org,omitempty" jsonschema:"GitLab group or GitHub organization"`
	State    string `json:"state,omitempty" jsonschema:"state: opened | closed | merged"`
	TopK     int    `json:"top_k,omitempty" jsonschema:"how many results to return (default 5, maximum 20)"`
}

type ciFailuresInput struct {
	Query  string `json:"query" jsonschema:"search query text"`
	Repo   string `json:"repo,omitempty" jsonschema:"repository path, or a unique short form (e.g. \"nats\", \"secrets/runner\")"`
	Branch string `json:"branch,omitempty" jsonschema:"branch"`
	TopK   int    `json:"top_k,omitempty" jsonschema:"how many results to return (default 5, maximum 20)"`
}

// resultsResponse is the structured payload of every search tool (T148):
// structuredContent must be a JSON object per the MCP spec, so the hits are
// wrapped into {"results": [...]} — a bare hit array is rejected by strict
// clients. The handlers keep the `any` output type so the shared structured
// toolError contract ({"code","message"}) stays untouched; a typed output
// would route the error path through the output-schema validation.
type resultsResponse[T any] struct {
	Results []T `json:"results"`
}

// --- response items (contracts/mcp-tools.md) ---

type codeHit struct {
	Repo      string  `json:"repo"`
	GroupOrg  string  `json:"group_org"`
	Branch    string  `json:"branch"`
	FilePath  string  `json:"file_path"`
	CommitSHA string  `json:"commit_sha"`
	Score     float32 `json:"score"`
	Snippet   string  `json:"snippet"`
}

type prHit struct {
	VCSType      string  `json:"vcs_type"`
	Repo         string  `json:"repo"`
	PRID         int64   `json:"pr_id"`
	SourceBranch string  `json:"source_branch"`
	TargetBranch string  `json:"target_branch"`
	State        string  `json:"state"`
	Score        float32 `json:"score"`
	Snippet      string  `json:"snippet"`
}

type issueHit struct {
	VCSType string  `json:"vcs_type"`
	Repo    string  `json:"repo"`
	IssueID int64   `json:"issue_id"`
	State   string  `json:"state"`
	Score   float32 `json:"score"`
	Snippet string  `json:"snippet"`
}

type ciHit struct {
	VCSType    string  `json:"vcs_type"`
	Repo       string  `json:"repo"`
	Branch     string  `json:"branch"`
	CommitSHA  string  `json:"commit_sha"`
	PipelineID string  `json:"pipeline_id"`
	JobName    string  `json:"job_name"`
	Score      float32 `json:"score"`
	Snippet    string  `json:"snippet"`
}

// registerTools registers the tools from contracts/mcp-tools.md.
func registerTools(server *mcp.Server, t *tools) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "search_code",
		Description: "Semantic search over indexed code. The result object is {\"results\": [hits]}.",
	}, t.searchCode)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "search_pull_requests",
		Description: "Semantic search over pull/merge requests. The result object is {\"results\": [hits]}.",
	}, t.searchPRs)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "search_issues",
		Description: "Semantic search over issues (indexing — US4/FR-012). The result object is {\"results\": [hits]}.",
	}, t.searchIssues)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "search_ci_failures",
		Description: "Semantic search over failed CI job logs. The result object is {\"results\": [hits]}.",
	}, t.searchCIFailures)
	// T113: registered only when NATS_URL is set (the refresh mode is on).
	if t.refresher != nil {
		mcp.AddTool(server, &mcp.Tool{
			Name: "refresh_repo",
			Description: "Refresh the index of one repository before searching: fetch + index new commits + metadata. " +
				"Use when the question depends on very recent changes. Blocks up to timeout_ms (default 20000); " +
				"if status=in_progress — the task continues in background: retry once or proceed with current data. " +
				"Do not call repeatedly for one repository.",
		}, t.refreshRepo)
	}
}

// searchCode — semantic code search (T046).
// Out=any: success — a []codeHit slice, error — toolError ({"error": {...}}).
func (t *tools) searchCode(ctx context.Context, _ *mcp.CallToolRequest, in searchCodeInput) (*mcp.CallToolResult, any, error) {
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()

	vec, err := t.queryVector(ctx, in.Query)
	if err != nil {
		return nil, errorResult(ctx, codeEmbedFailed, err), nil
	}
	f := store.Filter{VCSType: in.VCSType, GroupOrg: in.GroupOrg, Branch: in.Branch, Type: "code"}
	t.applyRepoFilter(ctx, &f, in.Repo, in.GroupOrg)
	points, err := t.searcher.Search(ctx, vec, f, clampTopK(in.TopK))
	if err != nil {
		return nil, errorResult(ctx, codeSearchFailed, err), nil
	}
	hits := make([]codeHit, 0, len(points))
	for _, p := range points {
		hits = append(hits, codeHit{
			Repo:      payloadString(p.Payload, "repo"),
			GroupOrg:  payloadString(p.Payload, "group_org"),
			Branch:    payloadString(p.Payload, "branch"),
			FilePath:  payloadString(p.Payload, "file_path"),
			CommitSHA: payloadString(p.Payload, "commit_sha"),
			Score:     p.Score,
			Snippet:   snippet(payloadString(p.Payload, "text")),
		})
	}
	return nil, resultsResponse[codeHit]{Results: hits}, nil
}

// searchPRs — MR/PR search (T071).
func (t *tools) searchPRs(ctx context.Context, _ *mcp.CallToolRequest, in metadataQueryInput) (*mcp.CallToolResult, any, error) {
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()

	vec, err := t.queryVector(ctx, in.Query)
	if err != nil {
		return nil, errorResult(ctx, codeEmbedFailed, err), nil
	}
	f := store.Filter{GroupOrg: in.GroupOrg, Type: "pull_request", State: in.State}
	t.applyRepoFilter(ctx, &f, in.Repo, in.GroupOrg)
	points, err := t.searcher.Search(ctx, vec, f, clampTopK(in.TopK))
	if err != nil {
		return nil, errorResult(ctx, codeSearchFailed, err), nil
	}
	hits := make([]prHit, 0, len(points))
	for _, p := range points {
		hits = append(hits, prHit{
			VCSType:      payloadString(p.Payload, "vcs_type"),
			Repo:         payloadString(p.Payload, "repo"),
			PRID:         payloadInt64(p.Payload, "pr_id"),
			SourceBranch: payloadString(p.Payload, "source_branch"),
			TargetBranch: payloadString(p.Payload, "target_branch"),
			State:        payloadString(p.Payload, "state"),
			Score:        p.Score,
			Snippet:      snippet(payloadString(p.Payload, "text")),
		})
	}
	return nil, resultsResponse[prHit]{Results: hits}, nil
}

// searchIssues — issue search (T072).
func (t *tools) searchIssues(ctx context.Context, _ *mcp.CallToolRequest, in metadataQueryInput) (*mcp.CallToolResult, any, error) {
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()

	vec, err := t.queryVector(ctx, in.Query)
	if err != nil {
		return nil, errorResult(ctx, codeEmbedFailed, err), nil
	}
	f := store.Filter{GroupOrg: in.GroupOrg, Type: "issue", State: in.State}
	t.applyRepoFilter(ctx, &f, in.Repo, in.GroupOrg)
	points, err := t.searcher.Search(ctx, vec, f, clampTopK(in.TopK))
	if err != nil {
		return nil, errorResult(ctx, codeSearchFailed, err), nil
	}
	hits := make([]issueHit, 0, len(points))
	for _, p := range points {
		hits = append(hits, issueHit{
			VCSType: payloadString(p.Payload, "vcs_type"),
			Repo:    payloadString(p.Payload, "repo"),
			IssueID: payloadInt64(p.Payload, "issue_id"),
			State:   payloadString(p.Payload, "state"),
			Score:   p.Score,
			Snippet: snippet(payloadString(p.Payload, "text")),
		})
	}
	return nil, resultsResponse[issueHit]{Results: hits}, nil
}

// searchCIFailures — search over failed CI job logs (T073).
func (t *tools) searchCIFailures(ctx context.Context, _ *mcp.CallToolRequest, in ciFailuresInput) (*mcp.CallToolResult, any, error) {
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()

	vec, err := t.queryVector(ctx, in.Query)
	if err != nil {
		return nil, errorResult(ctx, codeEmbedFailed, err), nil
	}
	f := store.Filter{Branch: in.Branch, Type: "ci_failure"}
	t.applyRepoFilter(ctx, &f, in.Repo, "")
	points, err := t.searcher.Search(ctx, vec, f, clampTopK(in.TopK))
	if err != nil {
		return nil, errorResult(ctx, codeSearchFailed, err), nil
	}
	hits := make([]ciHit, 0, len(points))
	for _, p := range points {
		hits = append(hits, ciHit{
			VCSType:    payloadString(p.Payload, "vcs_type"),
			Repo:       payloadString(p.Payload, "repo"),
			Branch:     payloadString(p.Payload, "branch"),
			CommitSHA:  payloadString(p.Payload, "commit_sha"),
			PipelineID: payloadString(p.Payload, "pipeline_id"),
			JobName:    payloadString(p.Payload, "job_name"),
			Score:      p.Score,
			Snippet:    snippet(payloadString(p.Payload, "text")),
		})
	}
	return nil, resultsResponse[ciHit]{Results: hits}, nil
}

// splitRepoName splits a full "namespace/path" name into the payload pair:
// group_org (the full namespace) and repo (the short path) — exactly as
// worker/index.go stores them (r.Namespace, r.Path). A name without "/" is a
// path-only repository (namespace empty).
func splitRepoName(full string) (namespace, path string) {
	if i := strings.LastIndex(full, "/"); i >= 0 {
		return full[:i], full[i+1:]
	}
	return "", full
}

// applyRepoFilter resolves the repo input and fills the filter fields; the
// auto-refresh decision runs on the resolved full name (T133-fix/T134).
func (t *tools) applyRepoFilter(ctx context.Context, f *store.Filter, raw, groupOrg string) {
	f.GroupOrg, f.Repo, f.RepoRefs = t.normalizeRepoFilter(ctx, raw, groupOrg)
}

// normalizeRepoFilter turns the raw repo input into the filter fields as
// stored in the payload (repo is the short path, group_org the namespace):
//   - one candidate — the direct pair (group_org=namespace, repo=short);
//   - 2–16 candidates — RepoRefs (OR of pairs), the loose group_org is dropped;
//   - none (no index / no match) — the raw value passthrough as before.
//
// The pair wins over a user-provided group_org: an exact match on group_org
// requires the full namespace, not the top-level group name.
func (t *tools) normalizeRepoFilter(ctx context.Context, raw, groupOrg string) (string, string, []store.RepoRef) {
	raw = strings.Trim(strings.TrimSpace(raw), "/")
	if raw == "" {
		return "", "", nil
	}
	var candidates []string
	if t.repos != nil {
		if cs, err := t.repos.resolve(ctx, raw); err == nil {
			candidates = cs
		}
	}
	if t.auto != nil {
		t.auto.beforeSearch(ctx, raw, candidates)
	}
	switch len(candidates) {
	case 0:
		return groupOrg, raw, nil
	case 1:
		ns, path := splitRepoName(candidates[0])
		if ns == "" {
			ns = groupOrg
		}
		return ns, path, nil
	default:
		candidates = clampCandidates(candidates)
		refs := make([]store.RepoRef, 0, len(candidates))
		for _, c := range candidates {
			ns, path := splitRepoName(c)
			refs = append(refs, store.RepoRef{GroupOrg: ns, Repo: path})
		}
		return "", "", refs
	}
}

// queryVector computes the embedding of the search query.
func (t *tools) queryVector(ctx context.Context, query string) ([]float32, error) {
	vecs, err := t.embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	if len(vecs) != 1 {
		return nil, fmt.Errorf("query embedding: got %d vectors, expected 1", len(vecs))
	}
	return vecs[0], nil
}

// clampTopK applies the default of 5 and the cap of 20 (contracts/mcp-tools.md).
func clampTopK(k int) int {
	switch {
	case k <= 0:
		return defaultTopK
	case k > maxTopK:
		return maxTopK
	default:
		return k
	}
}

// snippet truncates text to maxSnippet runes (without breaking UTF-8).
func snippet(text string) string {
	runes := []rune(text)
	if len(runes) <= maxSnippet {
		return text
	}
	return string(runes[:maxSnippet-1]) + "…"
}

// payloadString reads a string payload field.
func payloadString(p map[string]any, key string) string {
	s, _ := p[key].(string)
	return s
}

// payloadInt64 reads an integer payload field (int64 from Qdrant, with slack).
func payloadInt64(p map[string]any, key string) int64 {
	switch v := p[key].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case int32:
		return int64(v)
	case float64:
		return int64(v)
	default:
		return 0
	}
}

// marshalJSON — a test helper: deterministic JSON of a value.
func marshalJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("marshal error: %v", err)
	}
	return string(data)
}
