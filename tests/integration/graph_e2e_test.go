//go:build e2e

package integration

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorizond/vcs-rag/internal/graph"
)

// US1 e2e scenarios of the SDLC context graph (SC-001, SC-005, SC-007), the
// US2 file→MR scenario (SC-002) and the US3 people/planning filters (SC-003):
// the tests boot the production MCP server (`-mode=mcp`, stdio, as in e2e_us3)
// against the live stack from the e2e env and read the graph collection the
// worker graph lane filled. No fixture repo is written: the fixture identity of
// the live index is operator-supplied, and a scenario with an unset input skips
// cleanly.
//
// Env:
//
//	QDRANT_URL, EMBEDDINGS_BASE_URL   live stack (same gate as the other e2e tests)
//	GRAPH_E2E_REPO                    full namespace/path of an indexed repo
//	GRAPH_E2E_SYMBOL                  its canonical stored symbol (methods: Name#Receiver)
//	GRAPH_E2E_FILE                    its file with >=1 outgoing import
//	GRAPH_E2E_PACKAGE                 its package with >=1 importer
//	GRAPH_E2E_FOREIGN_REPO            second repo in the same graph collection (SC-007)
//	GRAPH_E2E_MR_FILE                 its file changed by an indexed MR/PR (SC-002)
//	GRAPH_E2E_MR_FILE_TRUNCATED       "1": require a patch_truncated chunk (SC-002)
//	GRAPH_E2E_ISSUE_STATE             canonical state of >=1 issue of the repo (SC-003)
//	GRAPH_E2E_PR_STATE                canonical state of >=1 MR/PR of the repo (SC-003)
//	GRAPH_E2E_ISSUE_LABEL/_AUTHOR/_MILESTONE  optional: pin those issue filters
//
// Example:
//
//	QDRANT_URL=http://localhost:16334 EMBEDDINGS_BASE_URL=http://localhost:11435 \
//	GRAPH_E2E_REPO=gorizond/vcs-rag GRAPH_E2E_SYMBOL=ProcessRepoOnce \
//	GRAPH_E2E_FILE=internal/worker/refresh.go GRAPH_E2E_PACKAGE=internal/worker \
//	GRAPH_E2E_FOREIGN_REPO=gorizond/other GRAPH_E2E_MR_FILE=internal/mcp/tools.go \
//	GRAPH_E2E_ISSUE_STATE=open GRAPH_E2E_PR_STATE=merged GRAPH_E2E_ISSUE_LABEL=bug \
//	go test -tags e2e ./tests/integration -run TestE2E_Graph -v -count=1
func TestE2E_Graph_SC001(t *testing.T) {
	proc := startGraphMCP(t)
	repo := graphFixture(t, "GRAPH_E2E_REPO")
	symbol := graphFixture(t, "GRAPH_E2E_SYMBOL")
	file := graphFixture(t, "GRAPH_E2E_FILE")
	pkg := graphFixture(t, "GRAPH_E2E_PACKAGE")

	// 1) the known symbol is found with its provenance (commit honesty).
	defs := decodeTool[gDefinitionsResp](t, graphCall(t, proc, 2, "search_definitions",
		map[string]any{"query": symbol, "repo": repo, "mode": "name", "top_k": 20}))
	if len(defs.Results) == 0 {
		t.Fatalf("search_definitions(%q, repo=%q, mode=name) returned nothing: is the fixture repo in the live graph index?", symbol, repo)
	}
	exact := 0
	for _, h := range defs.Results {
		if h.Repo != repo {
			t.Errorf("search_definitions: foreign repo %q in results (want %q)", h.Repo, repo)
		}
		if h.Symbol == symbol {
			exact++
			if h.FilePath == "" || h.CommitSHA == "" {
				t.Errorf("definition %q: file_path/commit_sha missing: %+v", symbol, h)
			}
		}
	}
	if exact == 0 {
		t.Errorf("search_definitions(%q): no exact definition among %d hits", symbol, len(defs.Results))
	}
	assertCoverage(t, "search_definitions", defs.Coverage, repo)
	t.Logf("read-back: search_definitions hits=%d exact=%d", len(defs.Results), exact)

	// 2) 1-hop dependents of the package: its importers.
	deps := decodeTool[gEdgesResp](t, graphCall(t, proc, 3, "get_dependents",
		map[string]any{"repo": repo, "package": pkg}))
	if len(deps.Results) == 0 {
		t.Errorf("get_dependents(repo=%q, package=%q): no importers", repo, pkg)
	}
	for _, e := range deps.Results {
		if e.FromFile == "" || e.FromPackage == "" || e.ToPackage == "" {
			t.Errorf("get_dependents: incomplete edge %+v", e)
		}
	}
	assertCoverage(t, "get_dependents", deps.Coverage, repo)
	t.Logf("read-back: get_dependents edges=%d", len(deps.Results))

	// 3) 1-hop imports of the known file.
	imps := decodeTool[gEdgesResp](t, graphCall(t, proc, 4, "get_imports",
		map[string]any{"repo": repo, "file_path": file}))
	if len(imps.Results) == 0 {
		t.Errorf("get_imports(repo=%q, file_path=%q): no imports", repo, file)
	}
	for _, e := range imps.Results {
		if e.FromFile != file {
			t.Errorf("get_imports: edge from %q, want %q", e.FromFile, file)
		}
	}
	assertCoverage(t, "get_imports", imps.Coverage, repo)
	t.Logf("read-back: get_imports edges=%d", len(imps.Results))
}

// TestE2E_Graph_SC002 — the file→MR lookup (US2): search_pull_requests with
// file_path returns the MRs that changed the file, each with per-file diff
// chunks. The patch_truncated invariant holds on every chunk; with
// GRAPH_E2E_MR_FILE_TRUNCATED=1 the fixture must exercise the truncated path.
func TestE2E_Graph_SC002(t *testing.T) {
	proc := startGraphMCP(t)
	repo := graphFixture(t, "GRAPH_E2E_REPO")
	file := graphFixture(t, "GRAPH_E2E_MR_FILE")

	resp := decodeTool[gPRFilesResp](t, graphCall(t, proc, 2, "search_pull_requests",
		map[string]any{"repo": repo, "file_path": file}))
	if len(resp.Results) == 0 {
		t.Fatalf("search_pull_requests(repo=%q, file_path=%q) returned no MRs: is the fixture file in an indexed MR?", repo, file)
	}
	mrs, chunks, truncated := 0, 0, 0
	for _, mr := range resp.Results {
		if mr.Repo != repo {
			t.Errorf("foreign repo %q in results (want %q)", mr.Repo, repo)
		}
		if mr.PRID == 0 {
			t.Errorf("MR result without pr_id: %+v", mr)
		}
		if len(mr.Files) == 0 {
			t.Errorf("MR %d (%s): no diff chunks", mr.PRID, repo)
		}
		mrs++
		for _, f := range mr.Files {
			if f.FilePath != file {
				t.Errorf("MR %d: chunk file_path %q, want %q", mr.PRID, f.FilePath, file)
			}
			if f.Status == "" {
				t.Errorf("MR %d: chunk without status: %+v", mr.PRID, f)
			}
			// The stored patch is capped at MaxPatchBytes; the flag is honest.
			if len(f.Patch) > graph.MaxPatchBytes {
				t.Errorf("MR %d: patch %d bytes > cap %d", mr.PRID, len(f.Patch), graph.MaxPatchBytes)
			}
			if f.PatchTruncated && len(f.Patch) > graph.MaxPatchBytes {
				t.Errorf("MR %d: truncated patch %d bytes > cap %d", mr.PRID, len(f.Patch), graph.MaxPatchBytes)
			}
			chunks++
			if f.PatchTruncated {
				truncated++
			}
		}
	}
	if truncated == 0 {
		msg := "no patch_truncated chunk in the fixture: the truncated path was not exercised"
		if os.Getenv("GRAPH_E2E_MR_FILE_TRUNCATED") == "1" {
			t.Errorf("%s (GRAPH_E2E_MR_FILE_TRUNCATED=1)", msg)
		} else {
			t.Logf("skip-note: %s", msg)
		}
	} else {
		t.Logf("read-back: patch_truncated chunks=%d (cap=%d bytes)", truncated, graph.MaxPatchBytes)
	}
	t.Logf("read-back: file=%q mrs=%d chunks=%d", file, mrs, chunks)
}

// TestE2E_Graph_SC003 — US3 (SC-003): issues/MRs filter by state/label/author/
// milestone, and the state vocabulary is canonical on both providers (GitLab
// "opened" → "open"; GitHub open|closed|merged|draft). The same scenario runs
// against either provider by switching the fixture env — one provider per
// instance, so the parity claim is checked on the active one: returned states
// must be in the canonical set and equal to the canonical request, never a raw
// provider spelling.
func TestE2E_Graph_SC003(t *testing.T) {
	proc := startGraphMCP(t)
	repo := graphFixture(t, "GRAPH_E2E_REPO")
	issueState := graphFixture(t, "GRAPH_E2E_ISSUE_STATE")
	prState := graphFixture(t, "GRAPH_E2E_PR_STATE")

	// issues: the canonical state plus the optional people/planning filters.
	args := map[string]any{"query": "issue", "repo": repo, "state": issueState, "top_k": 20}
	for env, key := range map[string]string{
		"GRAPH_E2E_ISSUE_LABEL":     "label",
		"GRAPH_E2E_ISSUE_AUTHOR":    "author",
		"GRAPH_E2E_ISSUE_MILESTONE": "milestone",
	} {
		if v := os.Getenv(env); v != "" {
			args[key] = v
		}
	}
	issues := decodeTool[gIssuesResp](t, graphCall(t, proc, 2, "search_issues", args))
	if len(issues.Results) == 0 {
		t.Fatalf("search_issues(repo=%q, state=%q, filters=%v) returned nothing: are the US3 fixtures indexed with normalized states?", repo, issueState, args)
	}
	for _, h := range issues.Results {
		if h.Repo != repo {
			t.Errorf("foreign repo %q in issue results (want %q)", h.Repo, repo)
		}
		if h.State != issueState || !canonicalStates[h.State] {
			t.Errorf("issue %d: state %q, want canonical %q", h.IssueID, h.State, issueState)
		}
	}

	// a value that cannot match must return nothing — the filters really
	// participate instead of being silently dropped.
	none := decodeTool[gIssuesResp](t, graphCall(t, proc, 3, "search_issues",
		map[string]any{"query": "issue", "repo": repo, "state": issueState,
			"label": "__e2e_no_such_label__", "author": "__e2e_no_such_author__",
			"milestone": "__e2e_no_such_milestone__"}))
	if len(none.Results) != 0 {
		t.Errorf("impossible label/author/milestone filters returned %d issues", len(none.Results))
	}

	// MR/PR: the same canonical state semantics on search_pull_requests.
	prs := decodeTool[gPRsResp](t, graphCall(t, proc, 4, "search_pull_requests",
		map[string]any{"query": "pull request", "repo": repo, "state": prState, "top_k": 20}))
	if len(prs.Results) == 0 {
		t.Fatalf("search_pull_requests(repo=%q, state=%q) returned nothing: is the fixture MR indexed with a canonical state?", repo, prState)
	}
	for _, h := range prs.Results {
		if h.Repo != repo {
			t.Errorf("foreign repo %q in PR results (want %q)", h.Repo, repo)
		}
		if h.State != prState || !canonicalStates[h.State] {
			t.Errorf("MR %d: state %q, want canonical %q", h.PRID, h.State, prState)
		}
	}
	t.Logf("read-back: issues=%d (state=%s), prs=%d (state=%s)", len(issues.Results), issueState, len(prs.Results), prState)
}

// TestE2E_Graph_SC005 — the graph self-describes: node/edge/entity types and
// the query DSL version come from graph_schema, not from the source code.
func TestE2E_Graph_SC005(t *testing.T) {
	proc := startGraphMCP(t)
	sch := decodeTool[gSchemaResp](t, graphCall(t, proc, 2, "graph_schema", map[string]any{}))

	if !hasType(sch.NodeTypes, "definition") {
		t.Errorf("graph_schema: node_types lacks definition: %+v", sch.NodeTypes)
	}
	for _, edge := range []string{"imports", "defines"} {
		if !hasType(sch.EdgeTypes, edge) {
			t.Errorf("graph_schema: edge_types lacks %q: %+v", edge, sch.EdgeTypes)
		}
	}
	for _, entity := range []string{"mr_file", "ci", "vuln"} {
		if !hasType(sch.EntityTypes, entity) {
			t.Errorf("graph_schema: entity_types lacks %q: %+v", entity, sch.EntityTypes)
		}
	}
	if sch.QueryDSL.Version == "" {
		t.Error("graph_schema: query_dsl.version is empty")
	}
	if len(sch.QueryDSL.Operators) == 0 {
		t.Error("graph_schema: query_dsl.operators is empty")
	}
	t.Logf("read-back: graph_schema dsl=%s operators=%d", sch.QueryDSL.Version, len(sch.QueryDSL.Operators))
}

// TestE2E_Graph_SC007 — instance isolation: a repo-scoped query returns only
// that repo's points, never a foreign repo's; skipped unless a second repo of
// the same collection is configured.
func TestE2E_Graph_SC007(t *testing.T) {
	proc := startGraphMCP(t)
	repo := graphFixture(t, "GRAPH_E2E_REPO")
	foreign := graphFixture(t, "GRAPH_E2E_FOREIGN_REPO")
	symbol := graphFixture(t, "GRAPH_E2E_SYMBOL")

	own := decodeTool[gDefinitionsResp](t, graphCall(t, proc, 2, "search_definitions",
		map[string]any{"query": symbol, "repo": repo, "mode": "name", "top_k": 20}))
	if len(own.Results) == 0 {
		t.Fatalf("search_definitions(repo=%q) returned nothing: is the fixture repo in the live graph index?", repo)
	}
	for _, h := range own.Results {
		if h.Repo != repo {
			t.Errorf("instance isolation violated: query on %q returned repo %q", repo, h.Repo)
		}
	}
	assertCoverage(t, "search_definitions", own.Coverage, repo)

	// The equivalent query on the foreign repo must return only its own points.
	other := decodeTool[gDefinitionsResp](t, graphCall(t, proc, 3, "search_definitions",
		map[string]any{"query": symbol, "repo": foreign, "mode": "name", "top_k": 20}))
	for _, h := range other.Results {
		if h.Repo != foreign {
			t.Errorf("instance isolation violated: query on %q returned repo %q", foreign, h.Repo)
		}
	}
	t.Logf("read-back: repo=%q hits=%d, foreign=%q hits=%d", repo, len(own.Results), foreign, len(other.Results))
}

// --- helpers ---

// startGraphMCP builds the binary, starts the production MCP server over stdio
// against the live stack from the e2e env, and initializes the session.
func startGraphMCP(t *testing.T) *mcpProcess {
	t.Helper()
	if os.Getenv("QDRANT_URL") == "" || os.Getenv("EMBEDDINGS_BASE_URL") == "" {
		t.Skip("set QDRANT_URL/EMBEDDINGS_BASE_URL to run graph e2e")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	proc := startMCP(t, buildBinary(t, ctx), []string{
		// The provider must exist for config.Load; the scenarios pass full
		// namespace/path fixtures, so repoIndex resolution is best-effort. The
		// defaults keep it off the network (closed loopback port).
		"GITLAB_GROUP_ID=" + envOr("GITLAB_GROUP_ID", "1"),
		"GITLAB_TOKEN=" + envOr("GITLAB_TOKEN", "dummy"),
		"GITLAB_BASE_URL=" + envOr("GITLAB_BASE_URL", "http://127.0.0.1:19998"),
		"GITHUB_ORG=", "GITHUB_TOKEN=", // neutralize a GitHub env: exactly one provider
		"EMBEDDINGS_PROVIDER=" + envOr("EMBEDDINGS_PROVIDER", "ollama"),
		"EMBEDDINGS_BASE_URL=" + os.Getenv("EMBEDDINGS_BASE_URL"),
		"EMBEDDINGS_MODEL=" + envOr("EMBEDDINGS_MODEL", "bge-m3"),
		"EMBEDDINGS_DIM=" + envOr("EMBEDDINGS_DIM", "1024"),
		"QDRANT_URL=" + os.Getenv("QDRANT_URL"),
		"QDRANT_COLLECTION=" + envOr("QDRANT_COLLECTION", "vcs_rag_e2e"),
		"QDRANT_GRAPH_COLLECTION=" + envOr("QDRANT_GRAPH_COLLECTION", "vcs_rag_graph"),
	})
	t.Cleanup(func() { proc.stop(t) })

	if resp := proc.call(t, 1, "initialize", `{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"graph-e2e","version":"0"}}`); !strings.Contains(resp, `"vcs-rag"`) {
		t.Fatalf("initialize: %s", truncateForLog(resp, 300))
	}
	proc.notify(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	return proc
}

// graphFixture returns an env fixture value or skips the scenario: the live
// graph index identity is operator-supplied — no repo is written by the test.
func graphFixture(t *testing.T, name string) string {
	t.Helper()
	if v := os.Getenv(name); v != "" {
		return v
	}
	t.Skipf("%s is not set: this scenario needs a live graph index (see the file header)", name)
	return ""
}

// graphCall invokes one MCP tool over JSON-RPC stdio and returns the raw line.
func graphCall(t *testing.T, p *mcpProcess, id int, name string, args map[string]any) string {
	t.Helper()
	params, err := json.Marshal(map[string]any{"name": name, "arguments": args})
	if err != nil {
		t.Fatalf("%s: marshal params: %v", name, err)
	}
	return p.call(t, id, "tools/call", string(params))
}

// decodeTool unwraps a tools/call response into the tool's structuredContent.
func decodeTool[T any](t *testing.T, line string) T {
	t.Helper()
	var env struct {
		Result struct {
			StructuredContent json.RawMessage `json:"structuredContent"`
			IsError           bool            `json:"isError"`
		} `json:"result"`
	}
	var out T
	if err := json.Unmarshal([]byte(line), &env); err != nil {
		t.Fatalf("decode response: %v: %s", err, truncateForLog(line, 300))
	}
	if env.Result.IsError {
		t.Fatalf("tool error: %s", truncateForLog(line, 400))
	}
	if len(env.Result.StructuredContent) == 0 {
		t.Fatalf("response has no structuredContent: %s", truncateForLog(line, 400))
	}
	if err := json.Unmarshal(env.Result.StructuredContent, &out); err != nil {
		t.Fatalf("decode structuredContent: %v: %s", err, truncateForLog(string(env.Result.StructuredContent), 300))
	}
	return out
}

// assertCoverage checks that the per-repo coverage notice names the repo.
func assertCoverage(t *testing.T, tool string, cov []gCoverage, repo string) {
	t.Helper()
	if len(cov) == 0 {
		t.Errorf("%s: coverage is missing", tool)
		return
	}
	for _, c := range cov {
		if c.Repo == repo {
			return
		}
	}
	t.Errorf("%s: coverage does not name repo %q: %+v", tool, repo, cov)
}

// hasType reports whether names contains want.
func hasType(names []gNamed, want string) bool {
	for _, n := range names {
		if n.Name == want {
			return true
		}
	}
	return false
}

// --- local response mirrors (the response types live in the unexported
// internal/mcp package; the e2e decodes the wire JSON instead) ---

type gCoverage struct {
	Repo          string `json:"repo"`
	IndexedCommit string `json:"indexed_commit"`
	Truncated     bool   `json:"truncated"`
}

type gDefinitionHit struct {
	Repo       string `json:"repo"`
	FilePath   string `json:"file_path"`
	Symbol     string `json:"symbol"`
	SymbolKind string `json:"symbol_kind"`
	Package    string `json:"package"`
	CommitSHA  string `json:"commit_sha"`
}

type gDefinitionsResp struct {
	Results  []gDefinitionHit `json:"results"`
	Coverage []gCoverage      `json:"coverage"`
}

type gImportEdge struct {
	FromFile    string `json:"from_file"`
	FromPackage string `json:"from_package"`
	ToPackage   string `json:"to_package"`
	External    bool   `json:"external"`
}

type gEdgesResp struct {
	Results  []gImportEdge `json:"results"`
	Coverage []gCoverage   `json:"coverage"`
}

type gPRFileChunk struct {
	FilePath       string `json:"file_path"`
	Status         string `json:"status"`
	Patch          string `json:"patch"`
	PatchTruncated bool   `json:"patch_truncated"`
}

type gPRFileHit struct {
	Repo   string         `json:"repo"`
	PRID   int64          `json:"pr_id"`
	State  string         `json:"state"`
	Author string         `json:"author"`
	Files  []gPRFileChunk `json:"files"`
}

type gPRFilesResp struct {
	Results []gPRFileHit `json:"results"`
}

// gIssuesResp / gPRsResp — US3 issue/PR hits (SC-003).
type gIssueHit struct {
	Repo    string `json:"repo"`
	IssueID int64  `json:"issue_id"`
	State   string `json:"state"`
}

type gIssuesResp struct {
	Results []gIssueHit `json:"results"`
}

type gPRHit struct {
	Repo  string `json:"repo"`
	PRID  int64  `json:"pr_id"`
	State string `json:"state"`
}

type gPRsResp struct {
	Results []gPRHit `json:"results"`
}

// canonicalStates — the unified state vocabulary (data-model §7).
var canonicalStates = map[string]bool{
	"open": true, "merged": true, "closed": true, "locked": true, "draft": true,
}

type gNamed struct {
	Name string `json:"name"`
}

type gSchemaResp struct {
	NodeTypes   []gNamed `json:"node_types"`
	EdgeTypes   []gNamed `json:"edge_types"`
	EntityTypes []gNamed `json:"entity_types"`
	QueryDSL    struct {
		Version   string   `json:"version"`
		Operators []string `json:"operators"`
	} `json:"query_dsl"`
}
