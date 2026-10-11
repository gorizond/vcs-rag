//go:build e2e

// Package conformance is the Orbit parity conformance harness (T005,
// contracts/conformance-harness.md): a runnable, read-only proof of the graph
// surface. It drives a LIVE vcs-rag MCP server through every tool/call the
// parity surface needs and emits parity-report.json + parity-report.md.
// Nothing is written server-side: only tools/call queries.
//
// Transport: the production server registers the MCP streamable-HTTP handler
// (`MCP_TRANSPORT=sse`, internal/mcp/server.go runHTTP), so the harness speaks
// a minimal stdlib SSE + JSON-RPC 2.0 client: POST the endpoint with
// `Accept: application/json, text/event-stream`, read the `data:` frames, echo
// `Mcp-Session-Id`. With no reachable server the test skips cleanly and writes
// no report (the graph_e2e_test.go gate style: the live identity is
// operator-supplied, no fixture is written).
//
// Run:
//
//	GRAPH_E2E_MCP_ADDR=127.0.0.1:18083 PARITY_REPORT_DIR=tests/conformance \
//	  go test -tags e2e ./tests/conformance -run TestOrbitParityConformance -v -count=1
//
// Env:
//
//	GRAPH_E2E_MCP_ADDR   host:port of the live MCP server (default 127.0.0.1:18083)
//	PARITY_REPORT_DIR    report output dir (default: repo root)
//	GRAPH_E2E_REPO       optional explicit repo fixture (tried first, before discovery)
//	GRAPH_E2E_SYMBOL     optional symbol for the search_definitions mode=name discovery
package conformance

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Row statuses of the parity report.
const (
	statusPass = "PASS"
	statusSkip = "SKIP"
	statusGap  = "GAP"
)

type parityRow struct {
	Area      string `json:"area"`
	OrbitItem string `json:"orbit_item"`
	VCSRag    string `json:"vcs_rag"`
	Status    string `json:"status"`
	Evidence  string `json:"evidence"`
}

type parityTotals struct {
	Pass  int `json:"pass"`
	Skip  int `json:"skip"`
	Gap   int `json:"gap"`
	Total int `json:"total"`
}

type parityReport struct {
	GeneratedAt string       `json:"generated_at"`
	MCPAddr     string       `json:"mcp_addr"`
	DSLVersion  string       `json:"dsl_version"`
	Totals      parityTotals `json:"totals"`
	Rows        []parityRow  `json:"rows"`
}

func (r *parityReport) add(area, orbit, vcsrag, status, evidence string) {
	r.Rows = append(r.Rows, parityRow{Area: area, OrbitItem: orbit, VCSRag: vcsrag, Status: status, Evidence: evidence})
}

func (r *parityReport) finish() {
	r.Totals = parityTotals{Total: len(r.Rows)}
	for _, row := range r.Rows {
		switch row.Status {
		case statusPass:
			r.Totals.Pass++
		case statusSkip:
			r.Totals.Skip++
		case statusGap:
			r.Totals.Gap++
		}
	}
}

// --- minimal MCP streamable-HTTP client (stdlib only) ---

type mcpClient struct {
	addr      string
	url       string
	hc        *http.Client
	sessionID string
	proto     string
	nextID    int
}

// dialOrSkip connects to the live server: unreachable → skip (no live stack,
// no report is written).
func dialOrSkip(t *testing.T) *mcpClient {
	t.Helper()
	addr := envOr("GRAPH_E2E_MCP_ADDR", "127.0.0.1:18083")
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Skipf("live MCP server not reachable at %s: %v (run `vcs-rag -mode=mcp` with MCP_TRANSPORT=sse)", addr, err)
	}
	_ = conn.Close()

	c := &mcpClient{addr: addr, url: "http://" + addr + "/mcp", hc: &http.Client{Timeout: 90 * time.Second}}
	msg := c.call(t, "initialize", map[string]any{
		// 2025-11-25 is negotiated by the SDK's stateful streamable transport.
		"protocolVersion": "2025-11-25",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "orbit-parity-conformance", "version": "0"},
	})
	if e := rpcErrorOf(msg); e != "" {
		t.Fatalf("initialize: %s", e)
	}
	if r, ok := msg["result"].(map[string]any); ok {
		if v, _ := r["protocolVersion"].(string); v != "" {
			c.proto = v
		}
	}
	c.notify(t, "notifications/initialized", nil)
	return c
}

func (c *mcpClient) call(t *testing.T, method string, params any) map[string]any {
	t.Helper()
	c.nextID++
	body := map[string]any{"jsonrpc": "2.0", "id": c.nextID, "method": method}
	if params != nil {
		body["params"] = params
	}
	return c.roundTrip(t, body)
}

func (c *mcpClient) notify(t *testing.T, method string, params any) {
	t.Helper()
	body := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		body["params"] = params
	}
	c.roundTrip(t, body)
}

func (c *mcpClient) roundTrip(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal JSON-RPC %v: %v", body["method"], err)
	}
	req, err := http.NewRequest(http.MethodPost, c.url, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", c.sessionID)
	}
	if c.proto != "" {
		req.Header.Set("Mcp-Protocol-Version", c.proto)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		t.Fatalf("MCP POST %s (%v): %v", c.url, body["method"], err)
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.sessionID = sid
	}
	_, hasID := body["id"]
	switch resp.StatusCode {
	case http.StatusAccepted:
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	case http.StatusOK:
		if !hasID { // a notification: no response body to read
			_, _ = io.Copy(io.Discard, resp.Body)
			return nil
		}
		return readJSONRPC(t, resp)
	default:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		t.Fatalf("MCP POST %s (%v): HTTP %d: %s", c.url, body["method"], resp.StatusCode, strings.TrimSpace(string(b)))
		return nil
	}
}

// readJSONRPC reads one JSON-RPC message: an application/json body, or the
// text/event-stream the go-sdk server sends by default (the stream is drained
// to EOF before returning — closing early drops the session).
func readJSONRPC(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "application/json") {
		var m map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
			t.Fatalf("decode JSON-RPC response: %v", err)
		}
		return m
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 1<<20), 8<<20)
	var msg map[string]any
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(data)), &m); err != nil {
			t.Fatalf("SSE data frame is not JSON-RPC: %v: %.300s", err, data)
		}
		if m["id"] != nil || m["result"] != nil || m["error"] != nil {
			msg = m
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read SSE stream: %v", err)
	}
	if msg == nil {
		t.Fatalf("no JSON-RPC message in response (Content-Type %q)", resp.Header.Get("Content-Type"))
	}
	return msg
}

func rpcErrorOf(msg map[string]any) string {
	if msg == nil {
		return ""
	}
	if e, ok := msg["error"]; ok {
		return fmt.Sprintf("%v", e)
	}
	return ""
}

// toolResult is one tools/call outcome: the structuredContent payload, the
// isError flag, or the JSON-RPC-level error text.
type toolResult struct {
	structured json.RawMessage
	isError    bool
	rpcError   string
}

func (c *mcpClient) tool(t *testing.T, name string, args map[string]any) toolResult {
	t.Helper()
	msg := c.call(t, "tools/call", map[string]any{"name": name, "arguments": args})
	if e := rpcErrorOf(msg); e != "" {
		return toolResult{rpcError: e}
	}
	raw, err := json.Marshal(msg["result"])
	if err != nil {
		t.Fatalf("%s: marshal result: %v", name, err)
	}
	var env struct {
		StructuredContent json.RawMessage `json:"structuredContent"`
		IsError           bool            `json:"isError"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("%s: decode result envelope: %v", name, err)
	}
	return toolResult{structured: env.StructuredContent, isError: env.IsError}
}

// errorCode returns the shared structured tool-error code ({"code","message"}),
// "rpc_error"/"tool_error" for the transport-level failures, or "" on success.
func (r toolResult) errorCode() string {
	if r.rpcError != "" {
		return "rpc_error"
	}
	if r.isError {
		return "tool_error"
	}
	if len(r.structured) == 0 {
		return "no_structured_content"
	}
	var te struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(r.structured, &te)
	return te.Code
}

// evidence renders the outcome for a GAP row.
func (r toolResult) evidence() string {
	switch {
	case r.rpcError != "":
		return "json-rpc error: " + truncate(r.rpcError, 160)
	case r.isError:
		return "tool error: " + truncate(string(r.structured), 160)
	case len(r.structured) == 0:
		return "empty structuredContent"
	default:
		return truncate(string(r.structured), 200)
	}
}

func decodeStructured[T any](t *testing.T, r toolResult) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(r.structured, &out); err != nil {
		t.Fatalf("decode structuredContent: %v: %.300s", err, r.structured)
	}
	return out
}

// --- wire shapes (mirrored from internal/mcp/tools.go, decoded from JSON) ---

type namedSpec struct {
	Name string `json:"name"`
}

type schemaResponse struct {
	NodeTypes   []namedSpec `json:"node_types"`
	EdgeTypes   []namedSpec `json:"edge_types"`
	EntityTypes []namedSpec `json:"entity_types"`
	QueryDSL    struct {
		Version string   `json:"version"`
		Modes   []string `json:"modes"`
	} `json:"query_dsl"`
	CoveragePerRepo []struct {
		Repo string `json:"repo"`
	} `json:"coverage_per_repo"`
}

type aggResponse struct {
	Groups []struct {
		Key   string `json:"key"`
		Count uint64 `json:"count"`
	} `json:"groups"`
	Truncated bool `json:"truncated"`
}

type traversalResponse struct {
	Results   []map[string]any `json:"results"`
	Truncated bool             `json:"truncated"`
}

type pathResponse struct {
	Status    string   `json:"status"`
	Path      []string `json:"path"`
	Truncated bool     `json:"truncated"`
}

type neighborsResponse struct {
	Neighbors []struct {
		Key      string `json:"key"`
		Type     string `json:"type"`
		Resolved bool   `json:"resolved"`
	} `json:"neighbors"`
	Truncated bool `json:"truncated"`
}

type definitionsResponse struct {
	Results []struct {
		Repo     string `json:"repo"`
		FilePath string `json:"file_path"`
		Symbol   string `json:"symbol"`
		Package  string `json:"package"`
	} `json:"results"`
}

// repoNamesResponse is the repo-bearing shape of search_issues and
// search_pull_requests hits.
type repoNamesResponse struct {
	Results []struct {
		Repo string `json:"repo"`
	} `json:"results"`
}

type edgesResponse struct {
	Results []struct {
		FromFile    string `json:"from_file"`
		FromPackage string `json:"from_package"`
		ToPackage   string `json:"to_package"`
	} `json:"results"`
}

// --- static Orbit domain reference table (28 items) ---

// domainCase is one Orbit domain item mapped to the vcs-rag model. A required
// element is satisfied by any "a|b" alternative it lists; the row is PASS when
// every element is declared by graph_schema. A non-empty gap marks the item as
// not modelled (the reason is the evidence) — no such row should remain after
// the parity-gap feature lands.
type domainCase struct {
	area     string
	orbit    string
	mapping  string
	required []string
	gap      string
}

// orbitDomains is the Orbit-side reference list (Core, code, review, CI/CD,
// planning, security) mapped item by item to the declared Orbit-named
// node/edge families of the graph schema (data-model.md). Legacy aliases
// (mr_file, vuln) are accepted as alternatives so pre-feature instances keep
// their recorded baseline.
var orbitDomains = []domainCase{
	// Core
	{area: "Core", orbit: "Group", mapping: "node group (contains group/project)", required: []string{"group"}},
	{area: "Core", orbit: "Project", mapping: "node project (contains project/branch; in_project edges)", required: []string{"project"}},
	{area: "Core", orbit: "User", mapping: "node user", required: []string{"user"}},
	{area: "Core", orbit: "Note", mapping: "node note", required: []string{"note"}},
	// code
	{area: "code", orbit: "Branch", mapping: "node branch (contains directory, on_branch file/directory)", required: []string{"branch"}},
	{area: "code", orbit: "Commit", mapping: "node commit (kind=commit, key repo|commit:<sha>; merged_at_commit merge_request -> commit)", required: []string{"commit"}},
	{area: "code", orbit: "Definition", mapping: "node definition", required: []string{"definition"}},
	{area: "code", orbit: "File", mapping: "file = its definition nodes (node_key=repo|file:path); contains directory/file", required: []string{"definition"}},
	{area: "code", orbit: "Directory", mapping: "node directory (contains directory/file)", required: []string{"directory"}},
	{area: "code", orbit: "ImportedSymbol", mapping: "node imported_symbol (resolved=false pseudo target; edge imports file/imported_symbol)", required: []string{"imported_symbol"}},
	// review
	{area: "review", orbit: "MergeRequest", mapping: "node merge_request (has_diff/has_latest_diff, merged/reviewer)", required: []string{"merge_request"}},
	{area: "review", orbit: "MergeRequestDiff", mapping: "node merge_request_diff (has_file)", required: []string{"merge_request_diff"}},
	{area: "review", orbit: "MergeRequestDiffFile", mapping: "node merge_request_diff_file (legacy alias mr_file)", required: []string{"merge_request_diff_file|mr_file"}},
	// CI/CD: the ci family flattens Pipeline/Stage/Job/Deployment/Environment (ci_level)
	{area: "CI/CD", orbit: "Pipeline", mapping: "entity ci (pipeline_id on ci points)", required: []string{"ci"}},
	{area: "CI/CD", orbit: "Stage", mapping: "entity ci (ci_level=stage)", required: []string{"ci"}},
	{area: "CI/CD", orbit: "Job", mapping: "entity ci (ci_level=job)", required: []string{"ci"}},
	{area: "CI/CD", orbit: "Deployment", mapping: "entity ci (ci_level=deployment)", required: []string{"ci"}},
	{area: "CI/CD", orbit: "Environment", mapping: "entity ci (ci_level=environment)", required: []string{"ci"}},
	{area: "CI/CD", orbit: "Runner", mapping: "node runner + edge executed_by (ci -> runner)", required: []string{"runner", "executed_by"}},
	// planning
	{area: "planning", orbit: "WorkItem", mapping: "node work_item", required: []string{"work_item"}},
	{area: "planning", orbit: "Milestone", mapping: "node milestone", required: []string{"milestone"}},
	{area: "planning", orbit: "Label", mapping: "node label", required: []string{"label"}},
	// security
	{area: "security", orbit: "Finding", mapping: "node finding (legacy alias vuln); has_finding security_scan -> finding", required: []string{"finding|vuln"}},
	{area: "security", orbit: "SecurityScan", mapping: "node security_scan (in_pipeline security_scan -> ci)", required: []string{"security_scan"}},
	{area: "security", orbit: "Vulnerability", mapping: "node vulnerability (legacy alias vuln)", required: []string{"vulnerability|vuln"}},
	{area: "security", orbit: "VulnerabilityIdentifier", mapping: "node vulnerability_identifier", required: []string{"vulnerability_identifier"}},
	{area: "security", orbit: "VulnerabilityOccurrence", mapping: "node vulnerability_occurrence (legacy alias vuln); occurrence_of -> vulnerability", required: []string{"vulnerability_occurrence|vuln"}},
	{area: "security", orbit: "VulnerabilityScanner", mapping: "node vulnerability_scanner (scans -> project)", required: []string{"vulnerability_scanner"}},
}

var (
	requiredNodes = []string{"definition", "user", "note", "work_item", "milestone", "label", "runner"}
	// F13 (schema v4) appends the 9 literal-parity relations: the data-bearing
	// in_group/member_of pair plus the declared-only (dormant) CI/user joins.
	requiredEdges = []string{
		"imports", "defines", "authored", "commented", "assigned_to", "labelled", "in_milestone", "executed_by",
		"in_group", "member_of", "triggered", "has_head_pipeline", "approved", "updated_by", "last_edited_by", "child_of", "created_for_mr",
	}
	requiredEntities = []string{"mr_file", "ci", "vuln"}
	requiredModes    = []string{"traversal", "aggregation", "path_finding", "neighbors"}
)

// dataFamily is one data-section row: the canonical Orbit family entity and
// the legacy aliases still accepted as data holders while the canonical kind
// is not ingested yet (data-model.md aliases: mr_file -> merge_request_diff_file,
// vuln -> vulnerability). A family is PASS when its count is > 0 (canonical or
// alias), SKIP when every answered name reports 0 on this instance, and GAP
// when no name is supported (unsupported/error).
type dataFamily struct {
	canonical string
	aliases   []string
}

// dataFamilies covers every graph family: the Orbit-parity families this
// feature adds, then the families already ingested.
var dataFamilies = []dataFamily{
	{canonical: "group"},
	{canonical: "project"},
	{canonical: "branch"},
	{canonical: "directory"},
	{canonical: "file"},
	{canonical: "imported_symbol"},
	{canonical: "merge_request"},
	{canonical: "merge_request_diff"},
	{canonical: "merge_request_diff_file", aliases: []string{"mr_file"}},
	{canonical: "commit"},
	{canonical: "vulnerability", aliases: []string{"vuln"}},
	{canonical: "finding"},
	{canonical: "vulnerability_occurrence"},
	{canonical: "vulnerability_identifier"},
	{canonical: "security_scan"},
	{canonical: "vulnerability_scanner"},
	{canonical: "definition"},
	{canonical: "ci"},
	{canonical: "runner"},
	{canonical: "user"},
	{canonical: "note"},
	{canonical: "work_item"},
	{canonical: "milestone"},
	{canonical: "label"},
}

// --- the harness ---

func TestOrbitParityConformance(t *testing.T) {
	c := dialOrSkip(t)
	rep := &parityReport{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		MCPAddr:     c.addr,
	}

	// 1) graph_schema: the self-description every mapping is checked against.
	schemaOut := c.tool(t, "graph_schema", map[string]any{})
	schema := schemaResponse{}
	schemaOK := schemaOut.errorCode() == ""
	if schemaOK {
		schema = decodeStructured[schemaResponse](t, schemaOut)
	}
	rep.DSLVersion = schema.QueryDSL.Version
	present := map[string]bool{}
	for _, list := range [][]namedSpec{schema.NodeTypes, schema.EdgeTypes, schema.EntityTypes} {
		for _, n := range list {
			present[n.Name] = true
		}
	}
	for _, m := range schema.QueryDSL.Modes {
		present[m] = true
	}

	addSchemaRow := func(area, orbit, mapping string, required []string) {
		if !schemaOK {
			rep.add(area, orbit, mapping, statusGap, "graph_schema unavailable: "+schemaOut.errorCode())
			return
		}
		status, evidence := declareCoverage(required, present)
		rep.add(area, orbit, mapping, status, evidence)
	}
	addSchemaRow("schema", "graph_schema.node_types", "node types (Core/planning/code + runner)", requiredNodes)
	addSchemaRow("schema", "graph_schema.edge_types", "edge types (imports/defines + people/planning/CI edges)", requiredEdges)
	addSchemaRow("schema", "graph_schema.entity_types", "entity types (mr_file/ci/vuln)", requiredEntities)
	// The literal version is deliberately not pinned: the schema version is
	// raised alongside the parity families (v2 -> v3 in this feature), only a
	// non-empty version plus the four modes are contractual.
	if !schemaOK {
		rep.add("schema", "graph_schema.query_dsl", "query DSL modes", statusGap, "graph_schema unavailable: "+schemaOut.errorCode())
	} else if miss := missing(requiredModes, present); len(miss) > 0 || schema.QueryDSL.Version == "" {
		rep.add("schema", "graph_schema.query_dsl", "query DSL modes",
			statusGap, fmt.Sprintf("version=%q modes missing=%v", schema.QueryDSL.Version, miss))
	} else {
		rep.add("schema", "graph_schema.query_dsl", "query DSL modes", statusPass,
			fmt.Sprintf("version=%s, modes=%d/%d", schema.QueryDSL.Version, len(requiredModes), len(requiredModes)))
	}

	// 2) the Orbit domain reference table: mapped or GAP.
	for _, d := range orbitDomains {
		switch {
		case d.gap != "":
			rep.add(d.area, d.orbit, d.mapping, statusGap, d.gap)
		case !schemaOK:
			rep.add(d.area, d.orbit, d.mapping, statusGap, "graph_schema unavailable: "+schemaOut.errorCode())
		default:
			status, evidence := declareCoverage(d.required, present)
			if status == statusPass {
				evidence = "declared: " + strings.Join(d.required, ",") + " (" + evidence + ")"
			}
			rep.add(d.area, d.orbit, d.mapping, status, evidence)
		}
	}

	// 3) discovery: repo candidates and definition node keys.
	candidates := repoCandidates(t, c, schema)
	primary, primaryCount := "", uint64(0)
	for _, cand := range candidates {
		if n, _ := aggCount(t, c, cand, "definition", "repo"); n > 0 {
			primary, primaryCount = cand, n
			break
		}
	}
	// search_definitions (mode=name) is the first-choice key discovery; the
	// definition traversal below always runs and fills file_path/node_key.
	var keys, files []string
	fileSet := map[string]bool{}
	addKey := func(k string) {
		if k == "" {
			return
		}
		for _, have := range keys {
			if have == k {
				return
			}
		}
		keys = append(keys, k)
	}
	if sym := os.Getenv("GRAPH_E2E_SYMBOL"); sym != "" && primary != "" {
		if r := c.tool(t, "search_definitions", map[string]any{"query": sym, "repo": primary, "mode": "name", "top_k": 20}); r.errorCode() == "" {
			for _, h := range decodeStructured[definitionsResponse](t, r).Results {
				addKey(h.Repo + "|file:" + h.FilePath)
				if !fileSet[h.FilePath] {
					fileSet[h.FilePath] = true
					files = append(files, h.FilePath)
				}
			}
		}
	}

	// 4) query modes: traversal (definition -> file, file -> imports).
	var pkg string
	trOut := toolResult{}
	traversalPoints, importEdges := 0, 0
	if primary != "" {
		trOut = c.tool(t, "query_graph", map[string]any{
			"mode":  "traversal",
			"start": startArgs(primary, "definition"),
			"limit": 30,
		})
		if trOut.errorCode() == "" {
			tr := decodeStructured[traversalResponse](t, trOut)
			traversalPoints = len(tr.Results)
			for _, p := range tr.Results {
				if k, _ := p["node_key"].(string); k != "" {
					addKey(k)
				}
				if f, _ := p["file_path"].(string); f != "" && !fileSet[f] {
					fileSet[f] = true
					files = append(files, f)
				}
				if pkg == "" {
					pkg, _ = p["package"].(string)
				}
			}
		}
		impOut := c.tool(t, "query_graph", map[string]any{
			"mode":  "traversal",
			"start": startArgs(primary, "definition"),
			"edge":  map[string]any{"type": "imports", "direction": "outgoing"},
			"limit": 30,
		})
		if impOut.errorCode() == "" {
			importEdges = len(decodeStructured[traversalResponse](t, impOut).Results)
		} else {
			trOut = impOut
		}
	}
	switch {
	case primary == "":
		rep.add("query modes", "traversal (definition->file, file->imports)", "query_graph mode=traversal", statusSkip, "no repo discovered on this instance")
	case trOut.errorCode() != "":
		rep.add("query modes", "traversal (definition->file, file->imports)", "query_graph mode=traversal", statusGap, "tool error: "+trOut.errorCode())
	case traversalPoints+importEdges > 0:
		rep.add("query modes", "traversal (definition->file, file->imports)", "query_graph mode=traversal", statusPass,
			fmt.Sprintf("repo=%s definition points=%d, import edges=%d, node_keys=%d", primary, traversalPoints, importEdges, len(keys)))
	default:
		rep.add("query modes", "traversal (definition->file, file->imports)", "query_graph mode=traversal", statusSkip,
			fmt.Sprintf("repo=%s: 0 points and 0 import edges", primary))
	}

	// 5) query modes: aggregation (count group_by=package).
	if primary == "" {
		rep.add("query modes", "aggregation (count group_by)", "query_graph mode=aggregation", statusSkip, "no repo discovered on this instance")
	} else if out := c.tool(t, "query_graph", map[string]any{
		"mode": "aggregation", "start": startArgs(primary, "definition"),
		"op": "count", "group_by": "package", "limit": 30,
	}); out.errorCode() != "" {
		rep.add("query modes", "aggregation (count group_by)", "query_graph mode=aggregation", statusGap, "tool error: "+out.errorCode())
	} else {
		res := decodeStructured[aggResponse](t, out)
		if len(res.Groups) > 0 {
			rep.add("query modes", "aggregation (count group_by)", "query_graph mode=aggregation", statusPass,
				fmt.Sprintf("repo=%s count by package = %d groups (first %q count=%d)", primary, len(res.Groups), res.Groups[0].Key, res.Groups[0].Count))
		} else {
			rep.add("query modes", "aggregation (count group_by)", "query_graph mode=aggregation", statusSkip,
				fmt.Sprintf("repo=%s: 0 groups", primary))
		}
	}

	// 6) query modes: path_finding (two known definition node keys).
	if len(keys) < 2 {
		rep.add("query modes", "path_finding (shortest)", "query_graph mode=path_finding", statusSkip,
			fmt.Sprintf("found %d definition node_keys (need >=2)", len(keys)))
	} else if out := c.tool(t, "query_graph", map[string]any{
		"mode": "path_finding", "from": keys[0], "to": keys[1],
		"rel_types": []string{"imports", "defines"}, "max_depth": 3,
	}); out.errorCode() != "" {
		rep.add("query modes", "path_finding (shortest)", "query_graph mode=path_finding", statusGap, "tool error: "+out.errorCode())
	} else {
		res := decodeStructured[pathResponse](t, out)
		switch res.Status {
		case "found", "not_found", "inconclusive":
			rep.add("query modes", "path_finding (shortest)", "query_graph mode=path_finding", statusPass,
				fmt.Sprintf("status=%s hops=%d from=%s to=%s", res.Status, len(res.Path), truncate(keys[0], 60), truncate(keys[1], 60)))
		default:
			rep.add("query modes", "path_finding (shortest)", "query_graph mode=path_finding", statusGap,
				fmt.Sprintf("unexpected status=%q", res.Status))
		}
	}

	// 7) query modes: neighbors (1-hop on a definition node_key).
	if len(keys) == 0 {
		rep.add("query modes", "neighbors (1-hop)", "query_graph mode=neighbors", statusSkip, "no definition node_key discovered")
	} else if out := c.tool(t, "query_graph", map[string]any{
		"mode": "neighbors", "node_key": keys[0], "direction": "both",
	}); out.errorCode() != "" {
		rep.add("query modes", "neighbors (1-hop)", "query_graph mode=neighbors", statusGap, "tool error: "+out.errorCode())
	} else {
		res := decodeStructured[neighborsResponse](t, out)
		resolved := 0
		for _, n := range res.Neighbors {
			if n.Resolved {
				resolved++
			}
		}
		rep.add("query modes", "neighbors (1-hop)", "query_graph mode=neighbors", statusPass,
			fmt.Sprintf("node_key=%s neighbors=%d resolved=%d", truncate(keys[0], 60), len(res.Neighbors), resolved))
	}

	// 8) data families: one aggregation count per family over the candidates.
	for _, fam := range dataFamilies {
		orbit := fam.canonical
		if len(fam.aliases) > 0 {
			orbit += " (alias " + strings.Join(fam.aliases, ",") + ")"
		}
		if len(candidates) == 0 {
			rep.add("data", "query_graph aggregation count "+orbit, "entity "+fam.canonical, statusSkip, "no repo discovered on this instance")
			continue
		}
		var (
			bestRepo string
			bestName string
			best     uint64
			zeroSeen bool   // at least one entity name answered with an empty count
			errCode  string // last unsupported/error code (none of the names answered)
		)
		names := append([]string{fam.canonical}, fam.aliases...)
		for _, name := range names {
			if best > 0 {
				break
			}
			for _, cand := range candidates {
				n, out := aggCount(t, c, cand, name, "repo")
				if code := out.errorCode(); code != "" {
					// An unknown entity is rejected at compile time,
					// independently of the repo: fall through to the next
					// name (legacy alias) instead of probing every candidate.
					errCode = code
					break
				}
				zeroSeen = true
				if n > 0 {
					best, bestRepo, bestName = n, cand, name
					break
				}
			}
		}
		switch {
		case best > 0:
			ev := fmt.Sprintf("count=%d repo=%s entity=%s", best, bestRepo, bestName)
			if bestName != fam.canonical {
				ev += " (alias)"
			}
			rep.add("data", "query_graph aggregation count "+orbit, "entity "+fam.canonical, statusPass, ev)
		case zeroSeen:
			ev := fmt.Sprintf("count=0 entity=%s across candidates %s", strings.Join(names, ","), strings.Join(candidates, ","))
			if errCode != "" {
				ev += " (alias probe error: " + errCode + ")"
			}
			rep.add("data", "query_graph aggregation count "+orbit, "entity "+fam.canonical, statusSkip, ev)
		default:
			rep.add("data", "query_graph aggregation count "+orbit, "entity "+fam.canonical, statusGap,
				fmt.Sprintf("entity=%s unsupported/error: %s", fam.canonical, errCode))
		}
	}

	// 9) tool surface: get_imports / get_dependents on discovered fixtures.
	addEdgeToolRow := func(tool string, args map[string]any) {
		if len(args) == 0 {
			rep.add("tools", tool, "graph edges", statusSkip, "no discovered file/package fixture on this instance")
			return
		}
		if out := c.tool(t, tool, args); out.errorCode() != "" {
			rep.add("tools", tool, "graph edges", statusGap, "tool error: "+out.errorCode())
		} else if res := decodeStructured[edgesResponse](t, out); len(res.Results) > 0 {
			rep.add("tools", tool, "graph edges (imports)", statusPass,
				fmt.Sprintf("edges=%d repo=%s args=%v", len(res.Results), primary, args))
		} else {
			rep.add("tools", tool, "graph edges (imports)", statusSkip, fmt.Sprintf("0 edges repo=%s args=%v", primary, args))
		}
	}
	importArgs := map[string]any{}
	if primary != "" && len(files) > 0 {
		importArgs = map[string]any{"repo": primary, "file_path": files[0]}
	}
	addEdgeToolRow("get_imports", importArgs)
	dependentsArgs := map[string]any{}
	if primary != "" {
		if pkg != "" {
			dependentsArgs = map[string]any{"repo": primary, "package": pkg}
		} else if len(files) > 0 {
			dependentsArgs = map[string]any{"repo": primary, "file_path": files[0]}
		}
	}
	addEdgeToolRow("get_dependents", dependentsArgs)

	// 10) negative: a query with no selective predicate is rejected.
	if out := c.tool(t, "query_graph", map[string]any{
		"mode": "traversal", "start": map[string]any{"entity": "definition"},
	}); out.errorCode() == "non_selective_query" {
		rep.add("negative", "non-selective query rejected", "query_graph selectivity guard", statusPass,
			"code=non_selective_query (before any scan)")
	} else {
		rep.add("negative", "non-selective query rejected", "query_graph selectivity guard", statusGap,
			"expected code=non_selective_query, got: "+firstNonEmpty(out.errorCode(), out.evidence()))
	}

	// 11) edge emission: observed by traversal from the source family over the
	// discovered candidates. has_file and in_pipeline are live on the canonical
	// node_key the legacy mr_file/ci points now carry (T016); executed_by is
	// the CI -> runner cross-domain link.
	observeEdge := func(area, orbit, entity, edge string) {
		if !present[edge] {
			rep.add(area, orbit, "edge "+edge, statusGap, edge+" is not declared by graph_schema")
			return
		}
		best, errCode := 0, ""
		for _, cand := range candidates {
			out := c.tool(t, "query_graph", map[string]any{
				"mode":  "traversal",
				"start": startArgs(cand, entity),
				"edge":  map[string]any{"type": edge, "direction": "outgoing"},
				"limit": 30,
			})
			if code := out.errorCode(); code != "" {
				errCode = code
				continue
			}
			if n := len(decodeStructured[traversalResponse](t, out).Results); n > 0 {
				best = n
				break
			}
		}
		switch {
		case best > 0:
			rep.add(area, orbit, "edge "+edge+" (traversal)", statusPass,
				fmt.Sprintf("traversed %s points=%d", entity, best))
		case errCode != "":
			rep.add(area, orbit, "edge "+edge+" (traversal)", statusGap, "tool error: "+errCode)
		default:
			rep.add(area, orbit, "edge "+edge+" (traversal)", statusSkip,
				"0 "+edge+" edges across candidates "+strings.Join(candidates, ","))
		}
	}
	observeEdge("cross-domain", "ci -> executed_by -> runner", "ci", "executed_by")
	observeEdge("data", "merge_request_diff -> has_file -> merge_request_diff_file", "merge_request_diff", "has_file")
	observeEdge("data", "security_scan -> in_pipeline -> ci", "security_scan", "in_pipeline")

	rep.finish()
	dir := writeReports(t, rep)
	for _, row := range rep.Rows {
		if row.Status == statusGap {
			t.Logf("GAP  %s / %s: %s", row.Area, row.OrbitItem, row.Evidence)
		}
	}
	t.Logf("parity: %d/%d PASS, %d SKIP, %d GAP (dsl=%s, repo=%s defs=%d) -> %s",
		rep.Totals.Pass, rep.Totals.Total, rep.Totals.Skip, rep.Totals.Gap, rep.DSLVersion, primary, primaryCount, dir)
}

// --- discovery and small helpers ---

func startArgs(repo, entity string) map[string]any {
	return map[string]any{"entity": entity, "filters": map[string]any{"repo": repo}}
}

// aggCount runs one aggregation count over repo+entity grouped by groupBy and
// returns the summed group count.
func aggCount(t *testing.T, c *mcpClient, repo, entity, groupBy string) (uint64, toolResult) {
	t.Helper()
	out := c.tool(t, "query_graph", map[string]any{
		"mode": "aggregation", "start": startArgs(repo, entity),
		"op": "count", "group_by": groupBy,
	})
	if out.errorCode() != "" {
		return 0, out
	}
	var n uint64
	for _, g := range decodeStructured[aggResponse](t, out).Groups {
		n += g.Count
	}
	return n, out
}

// repoCandidates discovers repos to probe, in priority order: the explicit env
// fixture, repos of a semantic search_definitions hit, repos of issue/MR hits
// (they carry the people/planning/review families), then the repos with
// coverage markers. Deduplicated, capped at 8; short repo forms are resolved
// against the coverage list.
func repoCandidates(t *testing.T, c *mcpClient, schema schemaResponse) []string {
	t.Helper()
	coverage := make([]string, 0, len(schema.CoveragePerRepo))
	for _, cov := range schema.CoveragePerRepo {
		coverage = append(coverage, cov.Repo)
	}
	var out []string
	seen := map[string]bool{}
	add := func(repo string) {
		repo = resolveRepo(strings.TrimSpace(repo), coverage)
		if repo == "" || seen[repo] || len(out) >= 8 {
			return
		}
		seen[repo] = true
		out = append(out, repo)
	}
	add(os.Getenv("GRAPH_E2E_REPO"))
	if r := c.tool(t, "search_definitions", map[string]any{"query": "code", "mode": "semantic", "top_k": 2}); r.errorCode() == "" {
		for _, h := range decodeStructured[definitionsResponse](t, r).Results {
			add(h.Repo)
		}
	}
	if r := c.tool(t, "search_issues", map[string]any{"query": "bug", "top_k": 5}); r.errorCode() == "" {
		for _, h := range decodeStructured[repoNamesResponse](t, r).Results {
			add(h.Repo)
		}
	}
	if r := c.tool(t, "search_pull_requests", map[string]any{"query": "fix", "top_k": 5}); r.errorCode() == "" {
		for _, h := range decodeStructured[repoNamesResponse](t, r).Results {
			add(h.Repo)
		}
	}
	for _, repo := range coverage {
		add(repo)
	}
	return out
}

// resolveRepo maps a short repo form (search hit) to its full coverage path.
func resolveRepo(repo string, coverage []string) string {
	if repo == "" {
		return ""
	}
	for _, full := range coverage {
		if full == repo {
			return repo
		}
	}
	for _, full := range coverage {
		if strings.HasSuffix(full, "/"+repo) {
			return full
		}
	}
	return repo
}

// declareCoverage maps required schema names to a status + evidence. Each
// element may list "a|b" alternatives (legacy aliases), satisfied by any.
func declareCoverage(required []string, present map[string]bool) (string, string) {
	var miss []string
	for _, req := range required {
		ok := false
		for _, name := range strings.Split(req, "|") {
			if present[name] {
				ok = true
				break
			}
		}
		if !ok {
			miss = append(miss, req)
		}
	}
	if len(miss) > 0 {
		return statusGap, fmt.Sprintf("missing %d/%d: %s (declared names=%d)", len(miss), len(required), strings.Join(miss, ","), len(present))
	}
	return statusPass, fmt.Sprintf("all %d required declared (names=%d)", len(required), len(present))
}

func missing(required []string, present map[string]bool) []string {
	var miss []string
	for _, n := range required {
		if !present[n] {
			miss = append(miss, n)
		}
	}
	return miss
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return "empty response"
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// --- report rendering ---

// writeReports writes parity-report.json and parity-report.md into
// PARITY_REPORT_DIR (default: the repo root) and returns the directory.
func writeReports(t *testing.T, rep *parityReport) string {
	t.Helper()
	dir := os.Getenv("PARITY_REPORT_DIR")
	if dir == "" {
		dir = repoRoot()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create report dir %s: %v", dir, err)
	}
	jb, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	jsonPath := filepath.Join(dir, "parity-report.json")
	if err := os.WriteFile(jsonPath, jb, 0o644); err != nil {
		t.Fatalf("write %s: %v", jsonPath, err)
	}
	mdPath := filepath.Join(dir, "parity-report.md")
	if err := os.WriteFile(mdPath, []byte(renderMarkdown(rep)), 0o644); err != nil {
		t.Fatalf("write %s: %v", mdPath, err)
	}
	return dir
}

func renderMarkdown(r *parityReport) string {
	var b strings.Builder
	b.WriteString("# Orbit parity conformance\n\n")
	fmt.Fprintf(&b, "Generated: %s | MCP: `%s` | graph DSL version: `%s`\n\n", r.GeneratedAt, r.MCPAddr, r.DSLVersion)
	b.WriteString("| Area | Orbit | vcs-rag | Status | Evidence |\n")
	b.WriteString("|---|---|---|---|---|\n")
	for _, row := range r.Rows {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n",
			mdCell(row.Area), mdCell(row.OrbitItem), mdCell(row.VCSRag), row.Status, mdCell(row.Evidence))
	}
	fmt.Fprintf(&b, "\n**%d/%d PASS, %d SKIP, %d GAP**\n", r.Totals.Pass, r.Totals.Total, r.Totals.Skip, r.Totals.Gap)
	b.WriteString("\n## Gaps\n\n")
	gaps := 0
	for _, row := range r.Rows {
		if row.Status != statusGap {
			continue
		}
		gaps++
		fmt.Fprintf(&b, "- **%s / %s** - %s (%s)\n", mdCell(row.Area), mdCell(row.OrbitItem), mdCell(row.Evidence), mdCell(row.VCSRag))
	}
	if gaps == 0 {
		b.WriteString("No GAP rows.\n")
	}
	return b.String()
}

func mdCell(s string) string {
	s = strings.ReplaceAll(s, "|", "/")
	return strings.ReplaceAll(s, "\n", " ")
}

// repoRoot walks up from the test working directory to the module root.
func repoRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return dir
		}
		dir = parent
	}
}
