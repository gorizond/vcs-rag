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
// The US1 parity modes of query_graph (T016) get their own scenarios:
// mode=aggregation counts the repo's MR points by author (SC001_Aggregation),
// mode=path_finding follows a live import edge of GRAPH_E2E_FILE
// (SC002_Path), mode=neighbors lists the 1-hop peers of its canonical file
// node_key in both directions (SC003_Neighbors). They need no new fixture envs.
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
//	GRAPH_E2E_VULN_FINDINGS           "1": the fixture's security API is available (SC-008 security)
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

// TestE2E_Graph_US5 — the query DSL end-to-end (SC-005 + R5 negatives): a
// simple node+edge+filter request returns structured points with a coverage
// notice; a query without a selective predicate is rejected with error code
// non_selective_query (no scan); a result-limit cut reports truncated:true
// instead of failing.
//
// Env: GRAPH_E2E_REPO + GRAPH_E2E_FILE (a file with >=1 outgoing import and
// >=1 definition — see the file header).
func TestE2E_Graph_US5(t *testing.T) {
	proc := startGraphMCP(t)
	repo := graphFixture(t, "GRAPH_E2E_REPO")
	file := graphFixture(t, "GRAPH_E2E_FILE")

	// 1) the simple DSL: the file's definitions and their outgoing imports.
	resp := decodeTool[gQueryResp](t, graphCall(t, proc, 2, "query_graph", map[string]any{
		"start": map[string]any{"entity": "definition", "filters": map[string]any{"repo": repo, "file_path": file}},
		"edge":  map[string]any{"type": "imports", "direction": "outgoing"},
		"hops":  1,
		"limit": 50,
	}))
	if len(resp.Results) == 0 {
		t.Fatalf("query_graph(repo=%q, file=%q) returned nothing: is the fixture file in the live graph index?", repo, file)
	}
	edges := 0
	for _, h := range resp.Results {
		if h.Repo != repo {
			t.Errorf("foreign repo %q in results (want %q)", h.Repo, repo)
		}
		if h.Kind == "edge" {
			if h.EdgeType != "imports" || h.FromFile != file {
				t.Errorf("edge result %+v, want imports from %q", h, file)
			}
			edges++
		}
	}
	if edges == 0 {
		t.Errorf("no import edge from %q among %d results", file, len(resp.Results))
	}
	assertCoverage(t, "query_graph", resp.Coverage, repo)
	if resp.Truncated && len(resp.Results) < 50 {
		t.Errorf("truncated=true with only %d results (< limit)", len(resp.Results))
	}
	t.Logf("read-back: query_graph points=%d edges=%d truncated=%v", len(resp.Results), edges, resp.Truncated)

	// 2) no selective predicate: rejected before any scan (R5).
	code, msg := decodeToolError(t, graphCall(t, proc, 3, "query_graph", map[string]any{
		"start": map[string]any{"entity": "definition", "filters": map[string]any{"branch": "main"}},
	}))
	if code != "non_selective_query" {
		t.Errorf("non-selective query code = %q, want non_selective_query (message: %s)", code, msg)
	}
	if !strings.Contains(msg, "repo") { // the message names the selective predicates
		t.Errorf("non-selective message does not name the selective predicates: %q", msg)
	}

	// 3) a limit cut is signaled, not failed (R5 caps).
	cut := decodeTool[gQueryResp](t, graphCall(t, proc, 4, "query_graph", map[string]any{
		"start": map[string]any{"entity": "definition", "filters": map[string]any{"repo": repo, "file_path": file}},
		"edge":  map[string]any{"type": "*", "direction": "outgoing"},
		"hops":  1,
		"limit": 1,
	}))
	if len(cut.Results) == 0 {
		t.Fatalf("query_graph(limit=1) returned nothing: is the fixture file in the live graph index?")
	}
	if !cut.Truncated {
		t.Errorf("limit=1 with %d points must report truncated=true: the fixture file needs >=2 graph points (definitions + edges)", len(cut.Results))
	}
	t.Logf("read-back: query_graph limit=1 points=%d truncated=%v", len(cut.Results), cut.Truncated)
}

// TestE2E_Graph_SC001_Aggregation — US1 parity e2e of mode=aggregation
// (SC-001): an exact grouped count of the fixture repo's MR/PR points by
// author. The answer is rows of {key,count,value}; with op=count value equals
// the group count, and every answer carries a coverage notice. Skipped when the
// fixture has no author-bearing MR/PR points (a code-only index).
//
// Env: GRAPH_E2E_REPO (see the file header).
func TestE2E_Graph_SC001_Aggregation(t *testing.T) {
	proc := startGraphMCP(t)
	repo := graphFixture(t, "GRAPH_E2E_REPO")

	line := graphCall(t, proc, 2, "query_graph", map[string]any{
		"mode":     "aggregation",
		"op":       "count",
		"group_by": "author",
		"start":    map[string]any{"entity": "mr_file", "filters": map[string]any{"repo": repo}},
		"limit":    100,
	})
	assertNoToolError(t, line)
	agg := decodeTool[gAggResp](t, line)
	assertCoverage(t, "query_graph(aggregation)", agg.Coverage, repo)
	if len(agg.Groups) == 0 {
		t.Skipf("aggregation(count by author) for %q returned no groups: the fixture index has no MR/PR points with an author", repo)
	}
	var total uint64
	for _, g := range agg.Groups {
		if g.Key == "" {
			t.Errorf("aggregation group with empty key: %+v", g)
		}
		if g.Count == 0 {
			t.Errorf("aggregation group %q: count=0", g.Key)
		}
		if g.Value != float64(g.Count) {
			t.Errorf("aggregation group %q: op=count value=%v, want %d", g.Key, g.Value, g.Count)
		}
		total += g.Count
	}
	if agg.Truncated && len(agg.Groups) < 100 {
		t.Errorf("truncated=true with only %d groups (< limit): an exact facet count is never cut silently", len(agg.Groups))
	}
	t.Logf("read-back: aggregation(count by author) groups=%d points=%d truncated=%v", len(agg.Groups), total, agg.Truncated)
}

// TestE2E_Graph_SC002_Path — US1 parity e2e of mode=path_finding (SC-002): the
// shortest path between the fixture file and one of its live import targets.
// The target is read back from the graph (a real to_key of an imports edge), so
// the pair is known-connected: the engine must answer found — a cap may make it
// inconclusive — never not_found, and never a hard failure. When the fixture
// file has no indexed import the scenario skips.
//
// Env: GRAPH_E2E_REPO + GRAPH_E2E_FILE (>=1 outgoing import).
func TestE2E_Graph_SC002_Path(t *testing.T) {
	proc := startGraphMCP(t)
	repo := graphFixture(t, "GRAPH_E2E_REPO")
	file := graphFixture(t, "GRAPH_E2E_FILE")

	// read one real endpoint from the live index: the file's first import edge.
	imps := decodeTool[gQueryResp](t, graphCall(t, proc, 2, "query_graph", map[string]any{
		"start": map[string]any{"entity": "definition", "filters": map[string]any{"repo": repo, "file_path": file}},
		"edge":  map[string]any{"type": "imports", "direction": "outgoing"},
		"limit": 50,
	}))
	from, to := graph.RefFile(repo, file), ""
	for _, h := range imps.Results {
		if h.Kind == "edge" && h.EdgeType == "imports" && h.ToKey != "" {
			to = h.ToKey
			break
		}
	}
	if to == "" {
		t.Skipf("no import edge from %q in the live graph: GRAPH_E2E_FILE must have >=1 outgoing import", file)
	}

	line := graphCall(t, proc, 3, "query_graph", map[string]any{
		"mode":      "path_finding",
		"from":      from,
		"to":        to,
		"type":      "shortest",
		"max_depth": 3,
		"rel_types": []string{"imports"},
	})
	assertNoToolError(t, line)
	path := decodeTool[gPathResp](t, line)
	switch path.Status {
	case "found", "not_found", "inconclusive":
	default:
		t.Fatalf("path_finding status %q outside found|not_found|inconclusive", path.Status)
	}
	if path.Status == "not_found" {
		t.Errorf("path_finding(%s → %s) = not_found although that import edge is indexed: an existing edge is found or, at worst, inconclusive", from, to)
	}
	if len(path.Path) > 0 {
		if path.Path[0] != from || path.Path[len(path.Path)-1] != to {
			t.Errorf("path endpoints %q → %q, want %q → %q", path.Path[0], path.Path[len(path.Path)-1], from, to)
		}
		if len(path.Edges) != len(path.Path)-1 {
			t.Errorf("path: %d nodes but %d edges (want nodes-1)", len(path.Path), len(path.Edges))
		}
		for i, e := range path.Edges {
			if e.FromKey != path.Path[i] || e.ToKey != path.Path[i+1] {
				t.Errorf("path edge %d %s → %s breaks the %q → %q chain", i, e.FromKey, e.ToKey, path.Path[i], path.Path[i+1])
			}
			if e.EdgeType == "" {
				t.Errorf("path edge %d without edge_type: %+v", i, e)
			}
		}
	}
	assertCoverage(t, "query_graph(path_finding)", path.Coverage, repo)
	t.Logf("read-back: path_finding status=%s nodes=%d edges=%d truncated=%v (%s → %s)", path.Status, len(path.Path), len(path.Edges), path.Truncated, from, to)
}

// TestE2E_Graph_SC003_Neighbors — US1 parity e2e of mode=neighbors (SC-003):
// the 1-hop peers of the fixture file's canonical node_key in both directions
// over the imports+defines relations. The fixture file has >=1 outgoing import,
// so the peer list is non-empty; every peer is either a resolved stored node or
// an honest pseudo-node, and the answer carries a coverage notice.
//
// Env: GRAPH_E2E_REPO + GRAPH_E2E_FILE (>=1 outgoing import).
func TestE2E_Graph_SC003_Neighbors(t *testing.T) {
	proc := startGraphMCP(t)
	repo := graphFixture(t, "GRAPH_E2E_REPO")
	file := graphFixture(t, "GRAPH_E2E_FILE")

	centre := graph.RefFile(repo, file)
	line := graphCall(t, proc, 2, "query_graph", map[string]any{
		"mode":      "neighbors",
		"node_key":  centre,
		"direction": "both",
		"rel_types": []string{"imports", "defines"},
		"limit":     100,
	})
	assertNoToolError(t, line)
	nb := decodeTool[gNeighborsResp](t, line)
	assertCoverage(t, "query_graph(neighbors)", nb.Coverage, repo)
	if len(nb.Neighbors) == 0 {
		t.Fatalf("neighbors(%q) returned no peers: the fixture file must have >=1 outgoing import", centre)
	}
	for _, n := range nb.Neighbors {
		if n.Key == "" || n.Key == centre {
			t.Errorf("neighbor with empty or self key %q", n.Key)
		}
		if n.Type == "" {
			t.Errorf("neighbor %q: type is empty", n.Key)
		}
		if len(n.EdgeTypes) == 0 {
			t.Errorf("neighbor %q: no edge_types", n.Key)
		}
		if n.Resolved {
			if n.Node == nil {
				t.Errorf("neighbor %q: resolved but no node payload", n.Key)
			} else if nk, _ := n.Node["node_key"].(string); nk != n.Key {
				t.Errorf("neighbor %q: resolved node carries node_key %q", n.Key, nk)
			}
		} else if n.Node != nil {
			t.Errorf("neighbor %q: unresolved but carries a node payload", n.Key)
		}
	}
	t.Logf("read-back: neighbors(%s) peers=%d truncated=%v", centre, len(nb.Neighbors), nb.Truncated)
}

// TestE2E_Graph_SC008_CI — US4 (SC-008 CI): the ci domain preserves the whole
// run history, not just failures. query_graph(entity:"ci") with a status filter
// finds both failed and successful runs; every job row carries job/duration
// (stage too when the provider has the concept — GitLab jobs do, GitHub
// Actions does not); deployment/environment rows are asserted when present.
// Skipped when the fixture repo has no ci points at all (provider without
// CIPipelineDetailProvider or a graph pass not run yet).
//
// Env: GRAPH_E2E_REPO (see the file header).
func TestE2E_Graph_SC008_CI(t *testing.T) {
	proc := startGraphMCP(t)
	repo := graphFixture(t, "GRAPH_E2E_REPO")

	baseLine := graphCall(t, proc, 2, "query_graph", map[string]any{
		"start": map[string]any{"entity": "ci", "filters": map[string]any{"repo": repo}},
		"limit": 100,
	})
	assertNoToolError(t, baseLine)
	base := decodeTool[gQueryCIResp](t, baseLine)
	if len(base.Results) == 0 {
		t.Skipf("no ci points for repo %q: the fixture has no CI domain indexed", repo)
	}
	assertCoverage(t, "query_graph(ci)", base.Coverage, repo)

	// SC-008: failed and success runs alike, each job row with stage/job/duration.
	for _, st := range []string{"failed", "success"} {
		line := graphCall(t, proc, 3, "query_graph", map[string]any{
			"start": map[string]any{"entity": "ci", "filters": map[string]any{"repo": repo, "status": st}},
			"limit": 100,
		})
		assertNoToolError(t, line)
		resp := decodeTool[gQueryCIResp](t, line)
		jobs := 0
		for _, h := range resp.Results {
			if h.Kind != "ci" || h.Repo != repo {
				t.Errorf("ci query on %q returned a foreign/invalid point: %+v", repo, h)
				continue
			}
			if h.Status != st || !canonicalCIStatuses[h.Status] {
				t.Errorf("status filter %q returned status %q (point %s)", st, h.Status, h.ID)
			}
			if h.CiLevel != "job" {
				continue
			}
			jobs++
			if h.JobName == "" || h.DurationMS == nil {
				t.Errorf("job row without job_name/duration_ms: %+v", h)
			}
			// GitLab carries the stage on the job; GitHub Actions has no stage concept.
			if h.VCSType == "gitlab" && h.Stage == "" {
				t.Errorf("gitlab job row without stage: %+v", h)
			}
		}
		if jobs == 0 {
			t.Errorf("status=%q: no ci_level=job rows among %d points of %q — the fixture needs a %s run with jobs", st, len(resp.Results), repo, st)
		}
		t.Logf("read-back: status=%s points=%d jobs=%d", st, len(resp.Results), jobs)
	}

	// deployment/environment rows: asserted when the provider exposes them.
	for _, level := range []string{"deployment", "environment"} {
		line := graphCall(t, proc, 4, "query_graph", map[string]any{
			"start": map[string]any{"entity": "ci", "filters": map[string]any{"repo": repo, "ci_level": level}},
			"limit": 100,
		})
		assertNoToolError(t, line)
		resp := decodeTool[gQueryCIResp](t, line)
		if len(resp.Results) == 0 {
			t.Logf("skip-note: no ci_level=%q rows for %q (optional for this provider)", level, repo)
			continue
		}
		for _, h := range resp.Results {
			if h.Kind != "ci" || h.CiLevel != level {
				t.Errorf("ci_level=%q query returned %+v", level, h)
			}
			if h.Env == "" {
				t.Errorf("ci_level=%q row without env: %+v", level, h)
			}
		}
		t.Logf("read-back: ci_level=%s rows=%d", level, len(resp.Results))
	}
}

// TestE2E_Graph_SC008_Security — US6 (SC-008 security): the security domain is
// conditional on the provider API/role. When the fixture declares availability
// (GRAPH_E2E_VULN_FINDINGS=1) query_graph(entity:"vuln") must return well-formed
// findings; otherwise an empty answer is the explicit "domain off" signal —
// error-free, the schema still declares the entity and the other domains keep
// working. Skipped only by the shared QDRANT_URL/EMBEDDINGS_BASE_URL gate.
//
// Env: GRAPH_E2E_REPO; optional GRAPH_E2E_VULN_FINDINGS=1.
func TestE2E_Graph_SC008_Security(t *testing.T) {
	proc := startGraphMCP(t)
	repo := graphFixture(t, "GRAPH_E2E_REPO")

	line := graphCall(t, proc, 2, "query_graph", map[string]any{
		"start": map[string]any{"entity": "vuln", "filters": map[string]any{"repo": repo}},
		"limit": 100,
	})
	assertNoToolError(t, line)
	resp := decodeTool[gVulnResp](t, line)

	findings := 0
	for _, h := range resp.Results {
		if h.Kind != "vuln" {
			continue // hop edges from the findings' location files may accompany the points
		}
		findings++
		if h.Repo != repo {
			t.Errorf("vuln query on %q returned repo %q", repo, h.Repo)
		}
		if !canonicalVulnSeverities[h.Severity] {
			t.Errorf("vuln %s: severity %q outside the canonical set", h.ID, h.Severity)
		}
		if !canonicalVulnStates[h.State] {
			t.Errorf("vuln %s: state %q outside the canonical set", h.ID, h.State)
		}
		if h.Scanner == "" {
			t.Errorf("vuln %s: scanner is empty: %+v", h.ID, h)
		}
	}
	if findings > 0 {
		assertCoverage(t, "query_graph(vuln)", resp.Coverage, repo)
		t.Logf("read-back: security domain available, findings=%d", findings)
		return
	}

	// The security domain is unavailable on this instance (403/404 or no
	// findings): the answer is an explicit empty one — assertNoToolError above
	// proves it is not an error.
	if os.Getenv("GRAPH_E2E_VULN_FINDINGS") == "1" {
		t.Fatalf("GRAPH_E2E_VULN_FINDINGS=1 but query_graph(entity:vuln) returned no findings for %q", repo)
	}
	assertCoverage(t, "query_graph(vuln, off)", resp.Coverage, repo)
	// Off, not gone: the schema still declares the entity type.
	sch := decodeTool[gSchemaResp](t, graphCall(t, proc, 3, "graph_schema", map[string]any{}))
	if !hasType(sch.EntityTypes, "vuln") {
		t.Errorf("security domain is off but graph_schema no longer declares the vuln entity: %+v", sch.EntityTypes)
	}
	// The rest of the graph lane keeps answering for the same repo.
	other := decodeTool[gQueryResp](t, graphCall(t, proc, 4, "query_graph", map[string]any{
		"start": map[string]any{"entity": "definition", "filters": map[string]any{"repo": repo}},
		"limit": 5,
	}))
	if len(other.Results) == 0 {
		t.Errorf("definition domain returned nothing for %q while the security domain is off: other domains must keep working", repo)
	}
	t.Logf("read-back: security domain unavailable (empty, no error); definitions still answer: %d points", len(other.Results))
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

// decodeToolError unwraps a structured tool error ({"code","message"} — the
// shared toolError contract; the Out=any handlers keep IsError unset, so the
// code lives in structuredContent).
func decodeToolError(t *testing.T, line string) (code, message string) {
	t.Helper()
	var env struct {
		Result struct {
			StructuredContent json.RawMessage `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(line), &env); err != nil {
		t.Fatalf("decode error response: %v: %s", err, truncateForLog(line, 300))
	}
	var te struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(env.Result.StructuredContent, &te); err != nil {
		t.Fatalf("decode structured error: %v: %s", err, truncateForLog(string(env.Result.StructuredContent), 300))
	}
	return te.Code, te.Message
}

// assertNoToolError fails when a tool answered with the shared structured error
// contract ({"code","message"}) instead of a result: the Out=any handlers keep
// IsError unset, so decodeTool alone would read such an error as an empty
// result. A malformed or empty response is left to decodeTool.
func assertNoToolError(t *testing.T, line string) {
	t.Helper()
	var env struct {
		Result struct {
			StructuredContent json.RawMessage `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(line), &env); err != nil || len(env.Result.StructuredContent) == 0 {
		return
	}
	var te struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(env.Result.StructuredContent, &te); err == nil && te.Code != "" {
		t.Fatalf("tool error: %s", truncateForLog(line, 400))
	}
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

// gQueryHit / gQueryResp — query_graph results (payload flattened + id).
type gQueryHit struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Repo      string `json:"repo"`
	EdgeType  string `json:"edge_type"`
	FromFile  string `json:"from_file"`
	ToPackage string `json:"to_package"`
	ToKey     string `json:"to_key"`
	Symbol    string `json:"symbol"`
	FilePath  string `json:"file_path"`
}

type gQueryResp struct {
	Results   []gQueryHit `json:"results"`
	Coverage  []gCoverage `json:"coverage"`
	Truncated bool        `json:"truncated"`
}

// gAggGroup / gAggResp — mode=aggregation rows (T016); value equals count for
// op=count.
type gAggGroup struct {
	Key   string  `json:"key"`
	Count uint64  `json:"count"`
	Value float64 `json:"value"`
}

type gAggResp struct {
	Groups    []gAggGroup `json:"groups"`
	Coverage  []gCoverage `json:"coverage"`
	Truncated bool        `json:"truncated"`
}

// gPathEdge / gPathResp — mode=path_finding (T016): the status is honest
// (found|not_found|inconclusive) and the path is the canonical key chain.
type gPathEdge struct {
	FromKey  string `json:"from_key"`
	ToKey    string `json:"to_key"`
	EdgeType string `json:"edge_type"`
}

type gPathResp struct {
	Status    string      `json:"status"`
	Path      []string    `json:"path"`
	Edges     []gPathEdge `json:"edges"`
	Coverage  []gCoverage `json:"coverage"`
	Truncated bool        `json:"truncated"`
}

// gNeighbor / gNeighborsResp — mode=neighbors (T016): resolved peers carry
// their stored node; the rest are pseudo-nodes (resolved:false, no node).
type gNeighbor struct {
	Key       string         `json:"key"`
	Type      string         `json:"type"`
	Resolved  bool           `json:"resolved"`
	EdgeTypes []string       `json:"edge_types"`
	Node      map[string]any `json:"node"`
}

type gNeighborsResp struct {
	Neighbors []gNeighbor `json:"neighbors"`
	Coverage  []gCoverage `json:"coverage"`
	Truncated bool        `json:"truncated"`
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

// gQueryCIHit / gQueryCIResp — US4 ci points of query_graph (SC-008 CI);
// duration_ms is a pointer: presence is asserted, 0 is a legitimate duration.
type gQueryCIHit struct {
	ID         string   `json:"id"`
	Kind       string   `json:"kind"`
	Repo       string   `json:"repo"`
	VCSType    string   `json:"vcs_type"`
	CiLevel    string   `json:"ci_level"`
	Status     string   `json:"status"`
	JobName    string   `json:"job_name"`
	Stage      string   `json:"stage"`
	Env        string   `json:"env"`
	DurationMS *float64 `json:"duration_ms"`
}

type gQueryCIResp struct {
	Results  []gQueryCIHit `json:"results"`
	Coverage []gCoverage   `json:"coverage"`
}

// gVulnHit / gVulnResp — US6 vuln points of query_graph (SC-008 security).
type gVulnHit struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"`
	Repo         string `json:"repo"`
	Severity     string `json:"severity"`
	Scanner      string `json:"scanner"`
	State        string `json:"state"`
	LocationFile string `json:"location_file"`
}

type gVulnResp struct {
	Results  []gVulnHit  `json:"results"`
	Coverage []gCoverage `json:"coverage"`
}

// canonicalCIStatuses / canonicalVulnSeverities / canonicalVulnStates — the
// unified vocabularies of the ci/vuln payloads (data-model §5–6).
var (
	canonicalCIStatuses = map[string]bool{
		"success": true, "failed": true, "running": true, "pending": true, "canceled": true, "skipped": true,
	}
	canonicalVulnSeverities = map[string]bool{
		"critical": true, "high": true, "medium": true, "low": true, "info": true,
	}
	canonicalVulnStates = map[string]bool{
		"detected": true, "confirmed": true, "dismissed": true, "resolved": true,
	}
)

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
