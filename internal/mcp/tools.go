package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/qdrant/go-client/qdrant"

	"github.com/gorizond/vcs-rag/internal/graph"
	"github.com/gorizond/vcs-rag/internal/store"
)

// Searcher — a narrow store interface for the tools (mock in tests).
type Searcher interface {
	Search(ctx context.Context, vector []float32, f store.Filter, topK int) ([]store.ScoredPoint, error)
}

// GraphReader — a narrow read surface of the payload-only graph collection
// (T017–T019); *store.GraphStore satisfies it. The graph writer is the worker.
type GraphReader interface {
	ScrollAll(ctx context.Context, f *qdrant.Filter, limit uint32) ([]*qdrant.RetrievedPoint, error)
}

// errGraphStore — the graph store was not wired into the MCP server.
var errGraphStore = errors.New("graph store is not configured")

// Embedder — a narrow embeddings interface for the tools (mock in tests).
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

const (
	toolTimeout = 60 * time.Second // per-call limit (timeouts → code timeout)
	defaultTopK = 5
	maxTopK     = 20 // cap from contracts/mcp-tools.md
	maxSnippet  = 400

	defaultDefinitionsTopK = 10          // search_definitions default (contract)
	graphPageSize          = uint32(256) // graph scroll page size
)

// codeInvalidInput — argument validation errors of the graph tools (T017–T019).
const codeInvalidInput = "invalid_input"

// codeNonSelective — a graph query without a selective indexed predicate
// (T035/T037, R5): the compiler refuses it instead of scanning the collection.
const codeNonSelective = "non_selective_query"

// tools — MCP tool implementations on top of narrow interfaces.
type tools struct {
	embedder  Embedder
	searcher  Searcher
	refresher refresher    // nil when NATS_URL is empty (refresh_repo is off)
	inflight  *flightGroup // in-flight share of refresh_repo (T113)
	repos     *repoIndex   // nil — the repo filter is passed through as before (T132)
	auto      *autoClient  // nil when NATS_URL is empty — auto-refresh is off (T134)
	status    statusReader // nil when NATS_URL is empty — index_status reports unavailable (T151)

	graph          GraphReader // nil — graph tools report qdrant_unavailable (T017–T019)
	graphSchemaVer string      // WORKER_GRAPH_SCHEMA_VER, reported by graph_schema
}

// statusReader reads the worker indexing status from the instance KV (T151).
type statusReader interface {
	WorkerStatus() ([]byte, bool)
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
	Query     string `json:"query" jsonschema:"search query text"`
	Repo      string `json:"repo,omitempty" jsonschema:"repository path, or a unique short form (e.g. \"nats\", \"secrets/runner\")"`
	GroupOrg  string `json:"group_org,omitempty" jsonschema:"GitLab group or GitHub organization"`
	State     string `json:"state,omitempty" jsonschema:"canonical state: open | merged | closed | locked | draft"`
	Label     string `json:"label,omitempty" jsonschema:"filter: the issue/MR carries this label"`
	Author    string `json:"author,omitempty" jsonschema:"filter: author login"`
	Milestone string `json:"milestone,omitempty" jsonschema:"filter: milestone title"`
	FilePath  string `json:"file_path,omitempty" jsonschema:"search_pull_requests only: exact lookup of the MRs/PRs that changed this file path (repo required; query and top_k are not used)"`
	TopK      int    `json:"top_k,omitempty" jsonschema:"how many results to return (default 5, maximum 20)"`
}

type ciFailuresInput struct {
	Query  string `json:"query" jsonschema:"search query text"`
	Repo   string `json:"repo,omitempty" jsonschema:"repository path, or a unique short form (e.g. \"nats\", \"secrets/runner\")"`
	Branch string `json:"branch,omitempty" jsonschema:"branch"`
	TopK   int    `json:"top_k,omitempty" jsonschema:"how many results to return (default 5, maximum 20)"`
}

// searchDefinitionsInput — T017.
type searchDefinitionsInput struct {
	Query      string `json:"query" jsonschema:"symbol name (name mode) or natural-language description (semantic mode)"`
	Repo       string `json:"repo,omitempty" jsonschema:"repository path, or a unique short form (e.g. \"nats\", \"secrets/runner\")"`
	FilePath   string `json:"file_path,omitempty" jsonschema:"filter: path of the file that defines the symbol"`
	SymbolKind string `json:"symbol_kind,omitempty" jsonschema:"filter: definition kind, e.g. func | method | type"`
	Mode       string `json:"mode,omitempty" jsonschema:"search mode: auto | name | semantic (default auto)"`
	TopK       int    `json:"top_k,omitempty" jsonschema:"how many results to return (default 10, maximum 20)"`
}

// graphEdgesInput — T018 (get_imports / get_dependents).
type graphEdgesInput struct {
	Repo      string `json:"repo" jsonschema:"repository required: import edges are namespaced by repository (full namespace/path or a unique short form)"`
	FilePath  string `json:"file_path,omitempty" jsonschema:"file path; give exactly one of file_path, package"`
	Package   string `json:"package,omitempty" jsonschema:"package path; give exactly one of file_path, package"`
	Direction string `json:"direction,omitempty" jsonschema:"traversal direction: outgoing | incoming (each tool has its own default)"`
}

// graphSchemaInput — T019 (no required arguments).
type graphSchemaInput struct {
	Repo string `json:"repo,omitempty" jsonschema:"optional repo scope for coverage_per_repo"`
}

// queryGraphInput — T037 (contracts/mcp-tools.md query_graph): start entity
// plus filters, an optional edge selector, hops/limit and global filters.
type queryGraphInput struct {
	Start   queryGraphStart `json:"start" jsonschema:"start entity (entity + filters)"`
	Edge    queryGraphEdge  `json:"edge,omitempty" jsonschema:"optional edge selector of the expansion"`
	Hops    int             `json:"hops,omitempty" jsonschema:"traversal depth: 1..3; 3 is the server cap (default 1)"`
	Limit   int             `json:"limit,omitempty" jsonschema:"result limit: default 30, maximum 1000"`
	Filters map[string]any  `json:"filters,omitempty" jsonschema:"global filters applied to the start and expansion points; at least one selective predicate (repo|file_path|from_file|to_package|from_package|symbol|mr_id) is required"`
}

// queryGraphStart — the DSL start selector: entity ∈ definition|file|mr_file|
// ci|vuln; filters are Qdrant operators on indexed payload fields.
type queryGraphStart struct {
	Entity  string         `json:"entity" jsonschema:"start entity: definition | file | mr_file | ci | vuln"`
	Filters map[string]any `json:"filters,omitempty" jsonschema:"start-point filters; must contain at least one selective indexed predicate"`
}

// queryGraphEdge — the DSL edge selector of the expansion.
type queryGraphEdge struct {
	Type      string `json:"type,omitempty" jsonschema:"edge type: imports | defines | * (default *)"`
	Direction string `json:"direction,omitempty" jsonschema:"traversal direction: outgoing | incoming (default outgoing)"`
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

// --- graph response items (T017–T019, contracts/mcp-tools.md) ---

// coverage — the per-repo graph coverage notice of every graph response
// (data-model §9, minimal form). Best-effort: indexed_commit stays empty until
// graph coverage points are written, truncated reports a shortened walk.
type coverage struct {
	Repo          string `json:"repo"`
	IndexedCommit string `json:"indexed_commit"`
	Truncated     bool   `json:"truncated"`
}

// definitionHit — one definition node; commit_sha is always carried (honesty).
type definitionHit struct {
	Repo       string  `json:"repo"`
	FilePath   string  `json:"file_path"`
	Symbol     string  `json:"symbol"`
	SymbolKind string  `json:"symbol_kind,omitempty"`
	Package    string  `json:"package,omitempty"`
	Signature  string  `json:"signature,omitempty"`
	CommitSHA  string  `json:"commit_sha"`
	Score      float32 `json:"score,omitempty"`
}

type definitionsResponse struct {
	Results  []definitionHit `json:"results"`
	Coverage []coverage      `json:"coverage"`
}

// importEdge — one edge_type=imports point.
type importEdge struct {
	FromFile    string `json:"from_file"`
	FromPackage string `json:"from_package"`
	ToPackage   string `json:"to_package"`
	External    bool   `json:"external"`
}

type importsResponse struct {
	Results  []importEdge `json:"results"`
	Coverage []coverage   `json:"coverage"`
}

// queryGraphResponse — T037: the traversal points (payload fields flattened,
// plus the point id), the per-repo coverage notice, and the truncation flag
// (an R5 cap cut the walk; the query itself still succeeded).
type queryGraphResponse struct {
	Results   []map[string]any `json:"results"`
	Coverage  []coverage       `json:"coverage"`
	Truncated bool             `json:"truncated"`
}

// graphSchemaResponse — the self-description of the graph collection (T019).
type graphSchemaResponse struct {
	NodeTypes       []nodeTypeSpec   `json:"node_types"`
	EdgeTypes       []edgeTypeSpec   `json:"edge_types"`
	EntityTypes     []entityTypeSpec `json:"entity_types"`
	QueryDSL        queryDSL         `json:"query_dsl"`
	CoveragePerRepo []schemaCoverage `json:"coverage_per_repo"`
}

type nodeTypeSpec struct {
	Name       string   `json:"name"`
	Properties []string `json:"properties"`
	Indexed    []string `json:"indexed"`
}

type edgeTypeSpec struct {
	Name string `json:"name"`
	From string `json:"from"`
	To   string `json:"to"`
}

type entityTypeSpec struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type queryDSL struct {
	Version   string   `json:"version"`
	Operators []string `json:"operators"`
}

// schemaCoverage — the full per-repo coverage of graph_schema (data-model §9).
type schemaCoverage struct {
	Repo                   string   `json:"repo"`
	IndexedCommit          string   `json:"indexed_commit"`
	Generation             int      `json:"generation"`
	IndexedAt              string   `json:"indexed_at"`
	LanguagesWithResolvers []string `json:"languages_with_resolvers"`
}

// --- worker indexing status (T151) ---

// workerStatusKVKey is the entry key of the worker_status_<id> KV (T150).
const workerStatusKVKey = "status"

// indexPassView mirrors the index pass counters of the worker status payload
// (T150; the write model lives in internal/worker/status.go).
type indexPassView struct {
	Repos          int64 `json:"repos"`
	OK             int64 `json:"ok"`
	Errors         int64 `json:"errors"`
	IndexedCommits int64 `json:"indexed_commits"`
	Resumed        int64 `json:"resumed"`
	Skipped        int64 `json:"skipped"`
	Pruned         int64 `json:"pruned"`
	DurationMS     int64 `json:"duration_ms"`
}

// workerStatusView is the worker_status_<id> KV payload (key "status").
type workerStatusView struct {
	ReposTotal    int            `json:"repos_total"`
	ReposIndexed  int            `json:"repos_indexed"`
	ReposPending  int            `json:"repos_pending"`
	PendingSample []string       `json:"pending_sample"`
	Points        *uint64        `json:"points,omitempty"` // omitted when the count failed
	LastPass      *indexPassView `json:"last_pass,omitempty"`
	UpdatedAt     string         `json:"updated_at"`
}

// indexStatusInput — index_status takes no arguments.
type indexStatusInput struct{}

// indexStatusResponse is the index_status answer: always a record. On success
// the status fields are flattened into the object; an unavailable answer
// carries the reason instead.
type indexStatusResponse struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
	*workerStatusView
}

// registerTools registers the tools from contracts/mcp-tools.md.
func registerTools(server *mcp.Server, t *tools) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "search_code",
		Description: "Semantic search over indexed code. The result object is {\"results\": [hits]}.",
	}, t.searchCode)
	mcp.AddTool(server, &mcp.Tool{
		Name: "search_pull_requests",
		Description: "Semantic search over pull/merge requests. The result object is {\"results\": [hits]}. " +
			"Optional filters: state (canonical open|merged|closed|locked|draft), label, author, milestone. " +
			"With file_path set, switches to an exact graph lookup instead: the MRs/PRs that changed that file, " +
			"each with its per-file diff chunks (file_path, status, patch, patch_truncated); repo is required " +
			"and query is not used in that mode.",
	}, t.searchPRs)
	mcp.AddTool(server, &mcp.Tool{
		Name: "search_issues",
		Description: "Semantic search over issues (indexing — US4/FR-012). The result object is {\"results\": [hits]}. " +
			"Optional filters: state (canonical open|merged|closed|locked|draft), label, author, milestone.",
	}, t.searchIssues)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "search_ci_failures",
		Description: "Semantic search over failed CI job logs. The result object is {\"results\": [hits]}.",
	}, t.searchCIFailures)
	// T017–T019: graph tools; they report qdrant_unavailable when no graph
	// store is wired (the production server always wires one).
	mcp.AddTool(server, &mcp.Tool{
		Name: "search_definitions",
		Description: "Search symbol definitions in the repository graph. mode=auto (default) tries name matching first and falls back " +
			"to the semantic code search when name finds nothing; mode=name matches the symbol exactly or as a full-text token — " +
			"there is NO substring or prefix operator (Qdrant match semantics). mode=semantic runs the vector code search and joins " +
			"the hits to definition nodes by (repo, file_path), keeping the max chunk score per file. Each result carries commit_sha; " +
			"coverage (repo, indexed_commit, truncated) is best-effort — indexed_commit is empty until the repository's first " +
			"completed graph scan. " +
			"The result object is {\"results\": [hits], \"coverage\": [...]}.",
	}, t.searchDefinitions)
	mcp.AddTool(server, &mcp.Tool{
		Name: "get_imports",
		Description: "Direct (1-hop) package imports of a file or package, from the graph edges (edge_type=imports). Package-level, " +
			"not symbol-level: one edge per imported package. direction=outgoing (default) lists what the input imports; " +
			"direction=incoming lists who imports the input package (a file input is resolved to its package(s) first). " +
			"package accepts the bare Go package name or a path (reduced to its last segment). " +
			"The result object is {\"results\": [{from_file, from_package, to_package, external}], \"coverage\": [...]}.",
	}, t.getImports)
	mcp.AddTool(server, &mcp.Tool{
		Name: "get_dependents",
		Description: "Direct (1-hop) dependents of a file or package from the graph edges: the importers. HONESTY: dependents of " +
			"file X are the importers of X's package(s), NOT symbol referencers of X — symbol-level references are not indexed " +
			"(package-level edges only). direction=incoming (default) lists importers of the input; direction=outgoing lists the " +
			"input's imports. package accepts both spellings: the bare Go package name (\"controllers\") or the import path " +
			"(\"github.com/x/y/pkg/controllers\") — either key form is matched. " +
			"The result object is {\"results\": [{from_file, from_package, to_package, external}], \"coverage\": [...]}.",
	}, t.getDependents)
	mcp.AddTool(server, &mcp.Tool{
		Name: "query_graph",
		Description: "Unified structured traversal (US5): start entity (definition|file|mr_file|ci|vuln) + filters, optional edge " +
			"selector (type imports|defines|*, direction outgoing|incoming), hops 1..3 (default 1), limit default 30 max 1000. " +
			"Filters are Qdrant operators on indexed payload fields: scalar = exact match, list = in/match-any, null = is_null, " +
			"{gt,gte,lt,lte} = numeric range. REQUIRED: at least one selective predicate " +
			"(repo|file_path|from_file|to_package|from_package|symbol|mr_id) — a filter on kind/branch alone is rejected with " +
			"error code non_selective_query instead of scanning. Caps cut the walk and set truncated=true. " +
			"The result object is {\"results\": [{...payload, id}], \"coverage\": [...], \"truncated\": bool}.",
	}, t.queryGraph)
	mcp.AddTool(server, &mcp.Tool{
		Name: "graph_schema",
		Description: "Self-describing graph schema: node types (definition) with indexed properties, edge types (imports, defines), " +
			"declared entity types (mr_file, ci, vuln), the query DSL version/operators, and coverage_per_repo " +
			"(repo, indexed_commit, generation, indexed_at, languages_with_resolvers) from the per-repo coverage markers — " +
			"best-effort, empty when no scan has completed yet.",
	}, t.graphSchema)
	// T151: registered unconditionally; without NATS it reports available=false.
	mcp.AddTool(server, &mcp.Tool{
		Name:        "index_status",
		Description: "Indexing progress: how many repositories are indexed, pending sample, points, last pass stats",
	}, t.indexStatus)
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

// metadataFilter builds the shared issue/PR filter (T031): optional canonical
// state, label, author and milestone — absent fields do not participate.
func metadataFilter(in metadataQueryInput, typ string) store.Filter {
	f := store.Filter{
		GroupOrg: in.GroupOrg, Type: typ, State: in.State,
		Author: in.Author, Milestone: in.Milestone,
	}
	if in.Label != "" {
		f.Labels = []string{in.Label}
	}
	return f
}

// searchPRs — MR/PR search (T071). With file_path the semantic search is
// replaced by the exact mr_file lookup (T026/SC-002).
func (t *tools) searchPRs(ctx context.Context, _ *mcp.CallToolRequest, in metadataQueryInput) (*mcp.CallToolResult, any, error) {
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()

	if filePath := strings.TrimSpace(in.FilePath); filePath != "" {
		return t.searchPRsByFile(ctx, in, filePath)
	}
	vec, err := t.queryVector(ctx, in.Query)
	if err != nil {
		return nil, errorResult(ctx, codeEmbedFailed, err), nil
	}
	f := metadataFilter(in, "pull_request")
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

// prFileHit — one MR/PR that changed the requested file, with its per-file
// diff chunks (the file_path mode of search_pull_requests, T026/SC-002).
type prFileHit struct {
	VCSType string        `json:"vcs_type"`
	Repo    string        `json:"repo"`
	PRID    int64         `json:"pr_id"`
	State   string        `json:"state"`
	Author  string        `json:"author,omitempty"`
	Files   []prFileChunk `json:"files"`
}

// prFileChunk — one stored mr_file point: the diff of one file of the MR.
type prFileChunk struct {
	FilePath       string `json:"file_path"`
	Status         string `json:"status"`
	Patch          string `json:"patch"`
	PatchTruncated bool   `json:"patch_truncated"`
}

// searchPRsByFile — the exact file lookup of search_pull_requests: mr_file
// points of repo+file_path grouped per MR, each with its diff chunk. The
// lookup is exact by design, so no embedding is computed and the query is not
// used (R2 honesty: this is not a semantic match).
func (t *tools) searchPRsByFile(ctx context.Context, in metadataQueryInput, filePath string) (*mcp.CallToolResult, any, error) {
	if t.graph == nil {
		return nil, errorResult(ctx, codeSearchFailed, errGraphStore), nil
	}
	repos := t.graphRepos(ctx, in.Repo)
	if len(repos) == 0 {
		return nil, toolError{Code: codeInvalidInput, Message: "repo is required: mr_file points are namespaced by repository"}, nil
	}
	f := &qdrant.Filter{Must: []*qdrant.Condition{
		qdrant.NewMatch("kind", string(graph.KindMrFile)),
		repoKeyword(repos),
		qdrant.NewMatch("file_path", filePath),
	}}
	if in.State != "" {
		f.Must = append(f.Must, qdrant.NewMatch("state", in.State))
	}
	pts, err := t.graph.ScrollAll(ctx, f, graphPageSize)
	if err != nil {
		return nil, errorResult(ctx, codeSearchFailed, err), nil
	}
	byMR := make(map[string]*prFileHit, len(pts))
	var order []string
	for _, p := range pts {
		m := payloadMap(p)
		key := fmt.Sprintf("%s#%d", payloadString(m, "repo"), payloadInt64(m, "mr_id"))
		h := byMR[key]
		if h == nil {
			h = &prFileHit{
				VCSType: payloadString(m, "vcs_type"),
				Repo:    payloadString(m, "repo"),
				PRID:    payloadInt64(m, "mr_id"),
				State:   payloadString(m, "state"),
				Author:  payloadString(m, "author"),
			}
			byMR[key] = h
			order = append(order, key)
		}
		h.Files = append(h.Files, prFileChunk{
			FilePath:       payloadString(m, "file_path"),
			Status:         payloadString(m, "status"),
			Patch:          payloadString(m, "patch"),
			PatchTruncated: payloadBool(m, "patch_truncated"),
		})
	}
	hits := make([]prFileHit, 0, len(order))
	for _, key := range order {
		h := byMR[key]
		sort.SliceStable(h.Files, func(i, j int) bool { return h.Files[i].FilePath < h.Files[j].FilePath })
		hits = append(hits, *h)
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Repo != hits[j].Repo {
			return hits[i].Repo < hits[j].Repo
		}
		return hits[i].PRID < hits[j].PRID
	})
	return nil, resultsResponse[prFileHit]{Results: hits}, nil
}

// searchIssues — issue search (T072).
func (t *tools) searchIssues(ctx context.Context, _ *mcp.CallToolRequest, in metadataQueryInput) (*mcp.CallToolResult, any, error) {
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()

	vec, err := t.queryVector(ctx, in.Query)
	if err != nil {
		return nil, errorResult(ctx, codeEmbedFailed, err), nil
	}
	f := metadataFilter(in, "issue")
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

// --- graph tools (T017–T019) ---

// searchDefinitions — symbol-definition search (T017). Name mode is a Qdrant
// filter (exact `symbol` OR full-text token `symbol_text`); semantic mode is a
// fallback vector code search joined to definitions on (repo, file_path).
func (t *tools) searchDefinitions(ctx context.Context, _ *mcp.CallToolRequest, in searchDefinitionsInput) (*mcp.CallToolResult, any, error) {
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()
	if t.graph == nil {
		return nil, errorResult(ctx, codeSearchFailed, errGraphStore), nil
	}
	if strings.TrimSpace(in.Query) == "" {
		return nil, toolError{Code: codeInvalidInput, Message: "query is required"}, nil
	}
	mode := in.Mode
	if mode == "" {
		mode = "auto"
	}
	if mode != "auto" && mode != "name" && mode != "semantic" {
		return nil, toolError{Code: codeInvalidInput, Message: `mode must be "auto", "name" or "semantic"`}, nil
	}
	topK := clampTopKDefault(in.TopK, defaultDefinitionsTopK)
	repos := t.graphRepos(ctx, in.Repo)

	if mode != "semantic" {
		hits, err := t.definitionNameHits(ctx, in, repos, topK)
		if err != nil {
			return nil, errorResult(ctx, codeSearchFailed, err), nil
		}
		if mode == "name" || len(hits) > 0 {
			return t.definitionsResult(ctx, repos, hits)
		}
	}
	vec, err := t.queryVector(ctx, in.Query)
	if err != nil {
		return nil, errorResult(ctx, codeEmbedFailed, err), nil
	}
	hits, err := t.definitionSemanticHits(ctx, vec, in, topK)
	if err != nil {
		return nil, errorResult(ctx, codeSearchFailed, err), nil
	}
	return t.definitionsResult(ctx, repos, hits)
}

// definitionsResult attaches the best-effort coverage notice (T057 markers);
// a repo without a marker keeps indexed_commit empty (unknown).
func (t *tools) definitionsResult(ctx context.Context, repos []string, hits []definitionHit) (*mcp.CallToolResult, any, error) {
	cov, err := t.coverageNotice(ctx, repos, hits)
	if err != nil {
		return nil, errorResult(ctx, codeSearchFailed, err), nil
	}
	return nil, definitionsResponse{Results: hits, Coverage: cov}, nil
}

// getImports — direct 1-hop imports of a file/package (T018): what the input
// imports (outgoing edges by default).
func (t *tools) getImports(ctx context.Context, _ *mcp.CallToolRequest, in graphEdgesInput) (*mcp.CallToolResult, any, error) {
	return t.graphEdges(ctx, in, string(graph.Outgoing))
}

// getDependents — direct dependents of a file/package (T018): who imports it.
// R2 honesty: for a file the answer is the importers of the file's package(s),
// never the symbol referencers (only package-level edges exist).
func (t *tools) getDependents(ctx context.Context, _ *mcp.CallToolRequest, in graphEdgesInput) (*mcp.CallToolResult, any, error) {
	return t.graphEdges(ctx, in, string(graph.Incoming))
}

// graphEdges — the shared 1-hop import-edge query. Outgoing filters by
// from_file/from_package; incoming filters by to_package (a file input is
// first resolved to the packages of its definition nodes).
func (t *tools) graphEdges(ctx context.Context, in graphEdgesInput, defaultDir string) (*mcp.CallToolResult, any, error) {
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()
	if t.graph == nil {
		return nil, errorResult(ctx, codeSearchFailed, errGraphStore), nil
	}
	repo := strings.Trim(strings.TrimSpace(in.Repo), "/")
	if repo == "" {
		return nil, toolError{Code: codeInvalidInput, Message: "repo is required: import edges are namespaced by repository"}, nil
	}
	if (in.FilePath == "") == (in.Package == "") {
		return nil, toolError{Code: codeInvalidInput, Message: "give exactly one of file_path, package"}, nil
	}
	dir := in.Direction
	if dir == "" {
		dir = defaultDir
	}
	if dir != string(graph.Outgoing) && dir != string(graph.Incoming) {
		return nil, toolError{Code: codeInvalidInput, Message: `direction must be "outgoing" or "incoming"`}, nil
	}
	repos := t.graphRepos(ctx, repo)
	f := importEdgesFilter(repos)
	switch dir {
	case string(graph.Outgoing):
		if in.FilePath != "" {
			f.Must = append(f.Must, qdrant.NewMatch("from_file", in.FilePath))
		} else {
			// from_package is the bare package name of the source file: any
			// spelling of the package argument reduces to that name.
			f.Must = append(f.Must, qdrant.NewMatchKeywords("from_package", packageKeys(repos, graph.PackageName(in.Package))...))
		}
	default: // incoming
		if in.Package != "" {
			f.Must = append(f.Must, dependentsCondition(repos, in.Package))
		} else {
			pkgs, err := t.filePackages(ctx, repos, in.FilePath)
			if err != nil {
				return nil, errorResult(ctx, codeSearchFailed, err), nil
			}
			if len(pkgs) == 0 {
				// No indexed definitions for the file → no package to look up:
				// nothing can match to_package_name (honest empty result, R2).
				cov, err := t.coverageNotice(ctx, repos, nil)
				if err != nil {
					return nil, errorResult(ctx, codeSearchFailed, err), nil
				}
				return nil, importsResponse{Results: []importEdge{}, Coverage: cov}, nil
			}
			// filePackages returns the bare package names of the file's
			// definition nodes — the to_package_name spelling of the edges.
			f.Must = append(f.Must, qdrant.NewMatchKeywords("to_package_name", pkgs...))
		}
	}
	edges, err := t.scrollEdges(ctx, f)
	if err != nil {
		return nil, errorResult(ctx, codeSearchFailed, err), nil
	}
	cov, err := t.coverageNotice(ctx, repos, nil)
	if err != nil {
		return nil, errorResult(ctx, codeSearchFailed, err), nil
	}
	return nil, importsResponse{Results: edges, Coverage: cov}, nil
}

// --- query_graph (T037) ---

// graphScroller adapts the tools' GraphReader to graph.Scroller (the same
// single method): the traversal stays pure logic over an injected scanner.
type graphScroller struct{ g GraphReader }

func (s graphScroller) ScrollAll(ctx context.Context, f *qdrant.Filter, limit uint32) ([]*qdrant.RetrievedPoint, error) {
	return s.g.ScrollAll(ctx, f, limit)
}

// queryGraph — the unified structured traversal (T037, FR-016/017): the DSL is
// compiled by graph.Compile (selectivity guard + hops/limit caps) and executed
// by graph.Traverse over the graph store. A non-selective query is rejected
// with code non_selective_query BEFORE any scan (R5); a cap cut is reported as
// truncated:true, never as a failure.
func (t *tools) queryGraph(ctx context.Context, _ *mcp.CallToolRequest, in queryGraphInput) (*mcp.CallToolResult, any, error) {
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()
	if t.graph == nil {
		return nil, errorResult(ctx, codeSearchFailed, errGraphStore), nil
	}
	q, err := in.graphQuery()
	if err != nil {
		return nil, toolError{Code: codeInvalidInput, Message: err.Error()}, nil
	}
	// Compile explicitly: its validation must reject the query before any scan
	// (Traverse compiles internally too; this call only classifies the error).
	if _, err := graph.Compile(q); err != nil {
		if errors.Is(err, graph.ErrNonSelective) {
			return nil, errorResult(ctx, codeNonSelective, err), nil
		}
		return nil, toolError{Code: codeInvalidInput, Message: err.Error()}, nil
	}
	res, err := graph.Traverse(ctx, graphScroller{g: t.graph}, q, graph.Coverage{})
	if err != nil {
		return nil, errorResult(ctx, codeSearchFailed, err), nil
	}
	hits := make([]map[string]any, 0, len(res.Points))
	for _, p := range res.Points {
		m := payloadMap(p)
		m["id"] = queryPointID(p.GetId())
		hits = append(hits, m)
	}
	cov, err := t.coverageNotice(ctx, queryRepos(in, res.Points), nil)
	if err != nil {
		return nil, errorResult(ctx, codeSearchFailed, err), nil
	}
	return nil, queryGraphResponse{Results: hits, Coverage: cov, Truncated: res.Truncated}, nil
}

// graphQuery maps the wire DSL to graph.Query: the start and global filter maps
// become raw Qdrant conditions, the edge selector sets type/direction, hops and
// limit pass through (graph.Compile clamps them).
func (in queryGraphInput) graphQuery() (graph.Query, error) {
	start, err := queryConditions(in.Start.Filters)
	if err != nil {
		return graph.Query{}, err
	}
	global, err := queryConditions(in.Filters)
	if err != nil {
		return graph.Query{}, err
	}
	return graph.Query{
		StartEntity: graph.Entity(in.Start.Entity),
		Start:       graph.Filter{Conditions: start},
		Filter:      graph.Filter{Conditions: global},
		EdgeType:    graph.EdgeType(in.Edge.Type),
		Direction:   graph.Direction(in.Edge.Direction),
		Hops:        in.Hops,
		Limit:       in.Limit,
	}, nil
}

// queryConditions compiles a DSL filter map into Qdrant conditions, one per
// field (ANDed). Supported value forms (contracts/mcp-tools.md operators):
// string → exact match; bool → boolean match; number → integer match; list →
// match-any/in; null → is_null; {gt,gte,lt,lte} → numeric range. Fields are
// sorted so the compiled filter is deterministic.
func queryConditions(filters map[string]any) ([]*qdrant.Condition, error) {
	fields := make([]string, 0, len(filters))
	for field := range filters {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	out := make([]*qdrant.Condition, 0, len(fields))
	for _, field := range fields {
		c, err := queryCondition(field, filters[field])
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// queryCondition compiles one field value; an unsupported form is a validation
// error, never a silently dropped filter.
func queryCondition(field string, v any) (*qdrant.Condition, error) {
	switch t := v.(type) {
	case string:
		return qdrant.NewMatch(field, t), nil
	case bool:
		return qdrant.NewMatchBool(field, t), nil
	case float64:
		return qdrant.NewMatchInt(field, int64(t)), nil
	case nil:
		return qdrant.NewIsNull(field), nil
	case []any:
		return queryListCondition(field, t)
	case map[string]any:
		return queryRangeCondition(field, t)
	default:
		return nil, fmt.Errorf("filter %q: unsupported value type %T", field, v)
	}
}

// queryListCondition compiles the list form (in/match-any). An empty list
// becomes an empty MatchAny, which the compiler short-circuits into an empty
// result without a scan (T035).
func queryListCondition(field string, values []any) (*qdrant.Condition, error) {
	if len(values) == 0 {
		return qdrant.NewMatchKeywords(field), nil
	}
	if _, ok := values[0].(string); ok {
		ss := make([]string, 0, len(values))
		for _, v := range values {
			s, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("filter %q: list must be all strings or all numbers, got %T", field, v)
			}
			ss = append(ss, s)
		}
		return qdrant.NewMatchKeywords(field, ss...), nil
	}
	ns := make([]int64, 0, len(values))
	for _, v := range values {
		n, ok := v.(float64)
		if !ok {
			return nil, fmt.Errorf("filter %q: list must be all strings or all numbers, got %T", field, v)
		}
		ns = append(ns, int64(n))
	}
	return qdrant.NewMatchInts(field, ns...), nil
}

// queryRangeCondition compiles the numeric range form {gt,gte,lt,lte}; at
// least one bound is required and unknown operators are rejected (a typo must
// not silently widen the query).
func queryRangeCondition(field string, spec map[string]any) (*qdrant.Condition, error) {
	r := &qdrant.Range{}
	for key, dst := range map[string]**float64{"gt": &r.Gt, "gte": &r.Gte, "lt": &r.Lt, "lte": &r.Lte} {
		v, ok := spec[key]
		if !ok {
			continue
		}
		n, ok := v.(float64)
		if !ok {
			return nil, fmt.Errorf("filter %q.%s: expected a number, got %T", field, key, v)
		}
		*dst = qdrant.PtrOf(n)
	}
	if r.Gt == nil && r.Gte == nil && r.Lt == nil && r.Lte == nil {
		return nil, fmt.Errorf("filter %q: range needs at least one of gt, gte, lt, lte", field)
	}
	for key := range spec {
		switch key {
		case "gt", "gte", "lt", "lte":
		default:
			return nil, fmt.Errorf("filter %q: unknown range operator %q (want gt|gte|lt|lte)", field, key)
		}
	}
	return qdrant.NewRange(field, r), nil
}

// queryRepos collects the repos a query touched: the explicit repo filter (a
// string or a list) of the start and global filter maps plus the repos of the
// returned points — even an empty result then carries a coverage notice for
// the scoped repo (data-model §9: every graph answer carries coverage).
func queryRepos(in queryGraphInput, pts []*qdrant.RetrievedPoint) []string {
	seen := map[string]bool{}
	var repos []string
	add := func(repo string) {
		if repo != "" && !seen[repo] {
			seen[repo] = true
			repos = append(repos, repo)
		}
	}
	for _, filters := range []map[string]any{in.Start.Filters, in.Filters} {
		switch v := filters["repo"].(type) {
		case string:
			add(v)
		case []any:
			for _, e := range v {
				if s, ok := e.(string); ok {
					add(s)
				}
			}
		}
	}
	for _, p := range pts {
		add(payloadString(payloadMap(p), "repo"))
	}
	sort.Strings(repos)
	return repos
}

// queryPointID renders a point ID (UUID or numeric) for the response.
func queryPointID(id *qdrant.PointId) string {
	if id == nil {
		return ""
	}
	if u := id.GetUuid(); u != "" {
		return u
	}
	return fmt.Sprintf("%d", id.GetNum())
}

// graphSchema — the self-describing graph schema (T019). coverage_per_repo is
// filled from the per-repo coverage markers (T057); best-effort — empty when
// no scan has completed yet.
func (t *tools) graphSchema(ctx context.Context, _ *mcp.CallToolRequest, in graphSchemaInput) (*mcp.CallToolResult, any, error) {
	ver := t.graphSchemaVer
	if ver == "" {
		ver = "1" // default WORKER_GRAPH_SCHEMA_VER
	}
	cov := []schemaCoverage{}
	if t.graph != nil {
		marks, err := t.fetchCoverage(ctx, t.graphRepos(ctx, in.Repo))
		if err != nil {
			return nil, errorResult(ctx, codeSearchFailed, err), nil
		}
		cov = marks
	}
	return nil, graphSchemaResponse{
		NodeTypes: []nodeTypeSpec{{
			Name:       string(graph.NodeDefinition),
			Properties: []string{"node_type", "symbol", "symbol_text", "symbol_kind", "package", "file_path", "signature", "commit_sha"},
			Indexed:    []string{"node_type", "symbol", "symbol_text", "symbol_kind", "package", "file_path"},
		}},
		EdgeTypes: []edgeTypeSpec{
			{Name: string(graph.EdgeImports), From: "file", To: "package"},
			{Name: string(graph.EdgeDefines), From: "file", To: "symbol"},
		},
		EntityTypes: []entityTypeSpec{
			{Name: string(graph.KindMrFile), Description: "per-file MR/PR diffs (search_pull_requests file_path)"},
			{Name: string(graph.KindCi), Description: "declared — extraction not implemented yet"},
			{Name: string(graph.KindVuln), Description: "declared — extraction not implemented yet"},
		},
		QueryDSL: queryDSL{
			Version:   ver,
			Operators: []string{"match", "match_text", "in", "range", "is_null"},
		},
		CoveragePerRepo: cov,
	}, nil
}

// fetchCoverage reads the per-repo coverage markers (kind=coverage, T057); a
// non-empty repos list narrows the scan. Repos without a marker are absent
// from the result — callers keep indexed_commit empty for them (unknown).
func (t *tools) fetchCoverage(ctx context.Context, repos []string) ([]schemaCoverage, error) {
	f := &qdrant.Filter{Must: []*qdrant.Condition{
		qdrant.NewMatch("kind", string(graph.KindCoverage)),
	}}
	if len(repos) > 0 {
		f.Must = append(f.Must, repoKeyword(repos))
	}
	pts, err := t.graph.ScrollAll(ctx, f, graphPageSize)
	if err != nil {
		return nil, err
	}
	out := make([]schemaCoverage, 0, len(pts))
	for _, p := range pts {
		m := payloadMap(p)
		out = append(out, schemaCoverage{
			Repo:                   payloadString(m, "repo"),
			IndexedCommit:          payloadString(m, "indexed_commit"),
			Generation:             int(payloadInt64(m, "generation")),
			IndexedAt:              payloadString(m, "indexed_at"),
			LanguagesWithResolvers: payloadStringList(m, "languages_with_resolvers"),
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Repo < out[j].Repo })
	return out, nil
}

// coverageNotice builds the minimal coverage notice of one response: the
// requested repos when known, otherwise the repos of the hits; indexed_commit
// and truncated come from the repo's marker (a missing marker → empty
// indexed_commit; the marker carries no truncation flag yet → false).
func (t *tools) coverageNotice(ctx context.Context, repos []string, hits []definitionHit) ([]coverage, error) {
	if len(repos) == 0 {
		seen := map[string]bool{}
		for _, h := range hits {
			if h.Repo != "" && !seen[h.Repo] {
				seen[h.Repo] = true
				repos = append(repos, h.Repo)
			}
		}
		sort.Strings(repos)
	}
	if len(repos) == 0 {
		return []coverage{}, nil
	}
	marks, err := t.fetchCoverage(ctx, repos)
	if err != nil {
		return nil, err
	}
	byRepo := make(map[string]schemaCoverage, len(marks))
	for _, m := range marks {
		byRepo[m.Repo] = m
	}
	out := make([]coverage, 0, len(repos))
	for _, r := range repos {
		m, ok := byRepo[r]
		c := coverage{Repo: r}
		if ok {
			c.IndexedCommit = m.IndexedCommit
		}
		out = append(out, c)
	}
	return out, nil
}

// graphRepos resolves the raw repo input to the canonical full names used by
// the graph payload (repo = "namespace/path" there, unlike the vector payload
// pair); a value that does not resolve passes through as-is.
func (t *tools) graphRepos(ctx context.Context, raw string) []string {
	raw = strings.Trim(strings.TrimSpace(raw), "/")
	if raw == "" {
		return nil
	}
	if t.repos != nil {
		if cs, err := t.repos.resolve(ctx, raw); err == nil && len(cs) > 0 {
			return clampCandidates(cs)
		}
	}
	return []string{raw}
}

// repoKeyword — the repo condition of a graph filter: exact for one resolved
// name, MatchAny for an ambiguous resolution. Callers pass a non-empty list.
func repoKeyword(repos []string) *qdrant.Condition {
	if len(repos) == 1 {
		return qdrant.NewMatch("repo", repos[0])
	}
	return qdrant.NewMatchKeywords("repo", repos...)
}

// definitionFilter — the definition-node filter: kind=node, node_type=definition,
// plus the optional repo scope, file_path and symbol_kind.
func definitionFilter(repos []string, filePath, symbolKind string) *qdrant.Filter {
	must := []*qdrant.Condition{
		qdrant.NewMatch("kind", string(graph.KindNode)),
		qdrant.NewMatch("node_type", string(graph.NodeDefinition)),
	}
	if len(repos) > 0 {
		must = append(must, repoKeyword(repos))
	}
	if filePath != "" {
		must = append(must, qdrant.NewMatch("file_path", filePath))
	}
	if symbolKind != "" {
		must = append(must, qdrant.NewMatch("symbol_kind", symbolKind))
	}
	return &qdrant.Filter{Must: must}
}

// importEdgesFilter — the import-edge filter: edge_type=imports plus the repo scope.
func importEdgesFilter(repos []string) *qdrant.Filter {
	return &qdrant.Filter{Must: []*qdrant.Condition{
		qdrant.NewMatch("edge_type", string(graph.EdgeImports)),
		repoKeyword(repos),
	}}
}

// payloadMap — the payload of a retrieved graph point.
func payloadMap(p *qdrant.RetrievedPoint) map[string]any {
	return qdrant.ValueMapToMap(p.GetPayload())
}

// hitFromDefinition maps a definition node payload to the response hit.
func hitFromDefinition(m map[string]any) definitionHit {
	return definitionHit{
		Repo:       payloadString(m, "repo"),
		FilePath:   payloadString(m, "file_path"),
		Symbol:     payloadString(m, "symbol"),
		SymbolKind: payloadString(m, "symbol_kind"),
		Package:    payloadString(m, "package"),
		Signature:  payloadString(m, "signature"),
		CommitSHA:  payloadString(m, "commit_sha"),
	}
}

// definitionNameHits — name mode: exact `symbol` match OR `symbol_text`
// full-text token match (no substring/prefix operator exists). ScrollAll reads
// the whole match set (there is no first-N scroll); the set is cut to topK.
func (t *tools) definitionNameHits(ctx context.Context, in searchDefinitionsInput, repos []string, topK int) ([]definitionHit, error) {
	f := definitionFilter(repos, in.FilePath, in.SymbolKind)
	should := []*qdrant.Condition{
		qdrant.NewMatch("symbol", in.Query),
		qdrant.NewMatchText("symbol_text", in.Query),
	}
	f.Should = should
	f.MinShould = &qdrant.MinShould{Conditions: should, MinCount: 1}
	pts, err := t.graph.ScrollAll(ctx, f, graphPageSize)
	if err != nil {
		return nil, err
	}
	hits := make([]definitionHit, 0, min(len(pts), topK))
	for _, p := range pts {
		hits = append(hits, hitFromDefinition(payloadMap(p)))
	}
	// Exact symbol matches first; the scroll order (point ID) is not a
	// user-facing relevance order.
	sort.SliceStable(hits, func(i, j int) bool {
		ei, ej := hits[i].Symbol == in.Query, hits[j].Symbol == in.Query
		if ei != ej {
			return ei
		}
		if hits[i].Symbol != hits[j].Symbol {
			return hits[i].Symbol < hits[j].Symbol
		}
		return hits[i].FilePath < hits[j].FilePath
	})
	return hits[:min(len(hits), topK)], nil
}

// fileRef — the semantic join key: canonical graph repo + file path.
type fileRef struct {
	repo string
	file string
}

// fullRepo recomposes the graph's canonical repo ("namespace/path") from the
// vector payload pair (group_org + repo).
func fullRepo(groupOrg, repo string) string {
	if groupOrg == "" {
		return repo
	}
	return groupOrg + "/" + repo
}

// splitRefs deduplicates the join refs into repo and file lists (order kept).
func splitRefs(refs []fileRef) (repos, files []string) {
	seenR := make(map[string]bool, len(refs))
	seenF := make(map[string]bool, len(refs))
	for _, r := range refs {
		if !seenR[r.repo] {
			seenR[r.repo] = true
			repos = append(repos, r.repo)
		}
		if !seenF[r.file] {
			seenF[r.file] = true
			files = append(files, r.file)
		}
	}
	return repos, files
}

// definitionSemanticHits — semantic mode: vector code search, chunks mapped to
// files with the max score per file, then joined to definition nodes on
// (repo, file_path).
func (t *tools) definitionSemanticHits(ctx context.Context, vec []float32, in searchDefinitionsInput, topK int) ([]definitionHit, error) {
	f := store.Filter{Type: "code"}
	t.applyRepoFilter(ctx, &f, in.Repo, "")
	// Oversample: several chunks of one file may occupy the top slots.
	pull := max(2*topK, 2*defaultDefinitionsTopK)
	points, err := t.searcher.Search(ctx, vec, f, pull)
	if err != nil {
		return nil, err
	}
	scores := make(map[fileRef]float32)
	var files []fileRef
	for _, p := range points {
		file := payloadString(p.Payload, "file_path")
		if file == "" || (in.FilePath != "" && file != in.FilePath) {
			continue
		}
		ref := fileRef{repo: fullRepo(payloadString(p.Payload, "group_org"), payloadString(p.Payload, "repo")), file: file}
		if s, ok := scores[ref]; !ok {
			files = append(files, ref)
			scores[ref] = p.Score
		} else if p.Score > s {
			scores[ref] = p.Score
		}
	}
	if len(files) == 0 {
		return []definitionHit{}, nil
	}
	repos, paths := splitRefs(files)
	fNodes := &qdrant.Filter{Must: []*qdrant.Condition{
		qdrant.NewMatch("kind", string(graph.KindNode)),
		qdrant.NewMatch("node_type", string(graph.NodeDefinition)),
		repoKeyword(repos),
		qdrant.NewMatchKeywords("file_path", paths...),
	}}
	pts, err := t.graph.ScrollAll(ctx, fNodes, graphPageSize)
	if err != nil {
		return nil, err
	}
	hits := make([]definitionHit, 0, topK)
	for _, p := range pts {
		m := payloadMap(p)
		ref := fileRef{repo: payloadString(m, "repo"), file: payloadString(m, "file_path")}
		score, ok := scores[ref]
		if !ok || (in.SymbolKind != "" && payloadString(m, "symbol_kind") != in.SymbolKind) {
			continue
		}
		h := hitFromDefinition(m)
		h.Score = score
		hits = append(hits, h)
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		if hits[i].Repo != hits[j].Repo {
			return hits[i].Repo < hits[j].Repo
		}
		if hits[i].FilePath != hits[j].FilePath {
			return hits[i].FilePath < hits[j].FilePath
		}
		return hits[i].Symbol < hits[j].Symbol
	})
	if len(hits) > topK {
		hits = hits[:topK]
	}
	return hits, nil
}

// filePackages returns the distinct packages of a file's definition nodes;
// incoming callers namespace them to the repo-scoped to_package keys.
func (t *tools) filePackages(ctx context.Context, repos []string, filePath string) ([]string, error) {
	pts, err := t.graph.ScrollAll(ctx, definitionFilter(repos, filePath, ""), graphPageSize)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var pkgs []string
	for _, p := range pts {
		pkg := payloadString(payloadMap(p), "package")
		if pkg != "" && !seen[pkg] {
			seen[pkg] = true
			pkgs = append(pkgs, pkg)
		}
	}
	sort.Strings(pkgs)
	return pkgs, nil
}

// packageKeys namespaces one input package with every resolved repo (PkgKey is
// the repo-scoped canonical key of the edge endpoints).
func packageKeys(repos []string, pkg string) []string {
	if pkg == "" {
		return nil
	}
	keys := make([]string, 0, len(repos))
	for _, r := range repos {
		keys = append(keys, graph.PkgKey(r, pkg))
	}
	return keys
}

// dependentsCondition matches the importers of a package argument on both
// spellings the edges carry: a path-like argument (contains "/" or ".") matches
// the repo-namespaced full import path (to_package = PkgKey), and every
// argument matches its bare package name (to_package_name, the last path
// segment). ORing the forms keeps "controllers" and
// "github.com/x/y/pkg/controllers" equivalent.
func dependentsCondition(repos []string, arg string) *qdrant.Condition {
	conds := make([]*qdrant.Condition, 0, 2)
	if strings.ContainsAny(arg, "/.") {
		conds = append(conds, qdrant.NewMatchKeywords("to_package", packageKeys(repos, arg)...))
	}
	if name := graph.PackageName(arg); name != "" {
		conds = append(conds, qdrant.NewMatchKeywords("to_package_name", name))
	}
	return qdrant.NewFilterAsCondition(&qdrant.Filter{Should: conds})
}

// scrollEdges reads all matching import edges into the response shape.
func (t *tools) scrollEdges(ctx context.Context, f *qdrant.Filter) ([]importEdge, error) {
	pts, err := t.graph.ScrollAll(ctx, f, graphPageSize)
	if err != nil {
		return nil, err
	}
	out := make([]importEdge, 0, len(pts))
	for _, p := range pts {
		m := payloadMap(p)
		out = append(out, importEdge{
			FromFile:    payloadString(m, "from_file"),
			FromPackage: payloadString(m, "from_package"),
			ToPackage:   payloadString(m, "to_package"),
			External:    payloadBool(m, "external"),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].FromFile != out[j].FromFile {
			return out[i].FromFile < out[j].FromFile
		}
		return out[i].ToPackage < out[j].ToPackage
	})
	return out, nil
}

// indexStatus — the indexing progress of this instance (T151): reads the
// worker_status_<id> KV; the tool is always registered and reports
// available=false when NATS is off or the worker has not published yet.
func (t *tools) indexStatus(_ context.Context, _ *mcp.CallToolRequest, _ indexStatusInput) (*mcp.CallToolResult, any, error) {
	if t.status == nil {
		return nil, indexStatusResponse{Available: false, Reason: "NATS is not configured"}, nil
	}
	data, ok := t.status.WorkerStatus()
	if !ok {
		return nil, indexStatusResponse{Available: false, Reason: "no worker status published yet"}, nil
	}
	var st workerStatusView
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, indexStatusResponse{Available: false, Reason: "worker status is unreadable"}, nil
	}
	return nil, indexStatusResponse{Available: true, workerStatusView: &st}, nil
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
func clampTopK(k int) int { return clampTopKDefault(k, defaultTopK) }

// clampTopKDefault applies a tool-specific default and the cap of 20.
func clampTopKDefault(k, def int) int {
	switch {
	case k <= 0:
		return def
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

// payloadBool reads a boolean payload field.
func payloadBool(p map[string]any, key string) bool {
	b, _ := p[key].(bool)
	return b
}

// payloadStringList reads a string-list payload field (Qdrant list values
// decode to []any).
func payloadStringList(p map[string]any, key string) []string {
	raw, _ := p[key].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// marshalJSON — a test helper: deterministic JSON of a value.
func marshalJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("marshal error: %v", err)
	}
	return string(data)
}
