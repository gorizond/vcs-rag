package graph

import (
	"errors"
	"fmt"

	"github.com/qdrant/go-client/qdrant"
)

// Query DSL caps (T034/T035, research R5). Hops above the ceiling are clamped
// (the DSL exposes 1..3: 3 is the server-side cap, UX is 1–2); the result limit
// defaults to 30 and never exceeds 1000.
const (
	maxHops      = 3
	defaultLimit = 30
	maxLimit     = 1000
)

// ErrNonSelective is returned by Compile for a query without a single
// selective indexed predicate. The message carries the contractual token
// `non_selective_query` (R5, contracts/graph-collection.md) so MCP callers can
// surface it verbatim. Such a query would be a full-collection scan.
var ErrNonSelective = errors.New("non_selective_query: at least one selective indexed predicate required (repo|file_path|from_file|to_package|from_package|symbol|mr_id|node_key|from_key|to_key|parent_key)")

// Orbit-parity start entities (T024/T032/T036). Their payload kind
// discriminator IS the entity name (kind=user, note, work_item, milestone,
// label, runner), so entityMatch maps them generically. Declared here with the
// start-entity surface of the DSL.
const (
	EntityUser      Entity = "user"
	EntityNote      Entity = "note"
	EntityWorkItem  Entity = "work_item"
	EntityMilestone Entity = "milestone"
	EntityLabel     Entity = "label"
	EntityRunner    Entity = "runner"
)

// ErrRelTypesRequired is returned by Compile for a path_finding query without
// rel_types: an unrestricted shortest-path search is rejected
// (contracts/mcp-tools.md, plan R3). Carries the contractual token
// `rel_types_required` so MCP callers can surface it verbatim.
var ErrRelTypesRequired = errors.New("rel_types_required: path_finding requires a non-empty rel_types")

// selectiveFields are the payload fields that bound a scan when present in a
// must chain. `kind`, `branch`, `group_org`, `package`, `status` and friends do
// NOT count: `kind=mr_file` alone still walks every mr_file point.
var selectiveFields = map[string]bool{
	"repo":         true,
	"file_path":    true,
	"from_file":    true,
	"to_package":   true,
	"from_package": true,
	"symbol":       true,
	"mr_id":        true,
	// Canonical-key endpoints (data-model §0): a node_key/from_key/to_key
	// condition is an exact lookup, never a full-collection scan.
	"node_key": true,
	"from_key": true,
	"to_key":   true,
	// parent_key (note → its work item/change request, data-model §2) bounds a
	// start scan to the notes of one parent — the natural note entry point.
	"parent_key": true,
}

// Compiled is the executable form of a Query: exact filters plus clamped
// bounds. Direction tells Traverse which edge endpoints the frontier keys map
// to on every hop.
type Compiled struct {
	Mode      Mode           // normalized query family (never empty)
	Start     *qdrant.Filter // exact filter of the start points
	Edge      *qdrant.Filter // per-hop edge filter before frontier narrowing
	Direction Direction      // expansion direction (outgoing|incoming)
	Hops      int            // clamped: 1..3 (path_finding: max depth)
	Limit     int            // clamped: default 30, max 1000
	// MatchNone reports that a must-chain MatchAny is empty: the query can
	// never match, so callers must return no points without a single request.
	MatchNone bool
}

// Compile turns a Query into the executable plan: the filter of the start
// points, the filter every expansion hop starts from, and the clamped bounds.
// Validation rules (T035):
//
//   - ≥1 selective predicate is required, else ErrNonSelective. Selectivity
//     comes from the typed Filter fields (repo, file_path, symbol), from raw
//     Conditions carrying a selective field in a must chain, or from the
//     canonical-key endpoints of the mode (neighbours node_key; path_finding
//     from+to); `should` and `must_not` never count — they cannot guarantee a
//     bound.
//   - An empty MatchAny in a must chain can never match: MatchNone is set and
//     callers short-circuit to an empty result without scanning. Lists longer
//     than the store's per-request ceiling are NOT chunked here: ScrollAll
//     expands them into one request per chunk (R5).
//   - hops is clamped to 1..3, limit to default 30 / max 1000.
//   - mode must be traversal|aggregation|path_finding|neighbors (empty
//     defaults to traversal); path_finding additionally requires a non-empty
//     rel_types, else ErrRelTypesRequired. This task (T010) only plumbs the
//     mode: the aggregation/path/neighbors engines are aggregate.go, path.go
//     and neighbors.go; the selectivity guard above holds for every mode.
//
// Start filter = entity discriminator + Start + Filter. Edge filter = kind=edge
// + pinned edge type + the fields every point kind shares (repo, group_org,
// vcs_type, branch) taken from both filters + the global raw conditions: an
// expansion must never leave the repository/branch scope, while start-specific
// predicates (symbol, file_path, ...) would wrongly exclude edges.
func Compile(q Query) (Compiled, error) {
	mode := q.Mode
	if mode == "" {
		mode = ModeTraversal
	}
	switch mode {
	case ModeTraversal, ModeAggregation, ModePathFinding, ModeNeighbors:
	default:
		return Compiled{}, fmt.Errorf("graph: invalid mode %q (want traversal|aggregation|path_finding|neighbors)", mode)
	}
	entity := q.StartEntity
	if entity == "" {
		entity = EntityDefinition
	}
	startConds, err := entityMatch(entity)
	if err != nil {
		return Compiled{}, err
	}
	dir := q.Direction
	if dir == "" {
		dir = Outgoing
	}
	if dir != Outgoing && dir != Incoming {
		return Compiled{}, fmt.Errorf("graph: invalid direction %q (want outgoing|incoming)", dir)
	}
	if q.EdgeType != "" && q.EdgeType != "*" && !knownEdgeType(q.EdgeType) {
		return Compiled{}, fmt.Errorf("graph: invalid edge type %q (want imports|defines|authored|commented|assigned_to|labelled|in_milestone|executed_by|*)", q.EdgeType)
	}
	if !selective(q.Start) && !selective(q.Filter) && !keyBounded(mode, q) {
		return Compiled{}, ErrNonSelective
	}
	if mode == ModePathFinding && len(q.RelTypes) == 0 {
		return Compiled{}, ErrRelTypesRequired
	}

	hops := q.Hops
	if hops < 1 {
		hops = 1
	} else if hops > maxHops {
		hops = maxHops
	}
	limit := q.Limit
	if limit <= 0 {
		limit = defaultLimit
	} else if limit > maxLimit {
		limit = maxLimit
	}

	c := Compiled{
		Mode:      mode,
		Start:     &qdrant.Filter{},
		Edge:      &qdrant.Filter{},
		Direction: dir,
		Hops:      hops,
		Limit:     limit,
	}
	c.Start.Must = append(c.Start.Must, startConds...)
	c.Start.Must = append(c.Start.Must, typedConditions(q.Start)...)
	c.Start.Must = append(c.Start.Must, q.Start.Conditions...)
	// The global Filter narrows the start points too (the DSL applies `filters`
	// to the entities the query is about).
	c.Start.Must = append(c.Start.Must, typedConditions(q.Filter)...)
	c.Start.Must = append(c.Start.Must, q.Filter.Conditions...)

	c.Edge.Must = append(c.Edge.Must, qdrant.NewMatch("kind", string(KindEdge)))
	if q.EdgeType != "" && q.EdgeType != "*" {
		c.Edge.Must = append(c.Edge.Must, qdrant.NewMatch("edge_type", string(q.EdgeType)))
	}
	c.Edge.Must = append(c.Edge.Must, scopeConditions(q.Start)...)
	c.Edge.Must = append(c.Edge.Must, scopeConditions(q.Filter)...)
	c.Edge.Must = append(c.Edge.Must, q.Filter.Conditions...)

	c.MatchNone = hasEmptyMatchAny(c.Start) || hasEmptyMatchAny(c.Edge)
	return c, nil
}

// keyBounded reports whether the mode's canonical-key endpoints bound the
// query: a neighbours centre node_key, or both endpoints of a path_finding
// request (data-model §0). Traversal has no key endpoint — its selectivity
// comes from the filters only, so a stray node_key never widens that guard.
func keyBounded(mode Mode, q Query) bool {
	switch mode {
	case ModeNeighbors:
		return q.NodeKey != ""
	case ModePathFinding:
		return q.FromKey != "" && q.ToKey != ""
	}
	return false
}

// knownEdgeType reports whether et is a declared relation label: the pinned
// edge-type filter must accept every EdgeType the collection can carry.
func knownEdgeType(et EdgeType) bool {
	switch et {
	case EdgeImports, EdgeDefines, EdgeAuthored, EdgeCommented,
		EdgeAssignedTo, EdgeLabelled, EdgeInMilestone, EdgeExecutedBy:
		return true
	}
	return false
}

// entityMatch is the kind discriminator of a start scanner: every entity
// family maps to the payload kind (and node_type) its points carry.
func entityMatch(e Entity) ([]*qdrant.Condition, error) {
	switch e {
	case EntityDefinition, EntityFile:
		// No standalone file points exist (R1 payload set): a file is its
		// definition nodes, so definition and file share the discriminator.
		return []*qdrant.Condition{
			qdrant.NewMatch("kind", string(KindNode)),
			qdrant.NewMatch("node_type", string(NodeDefinition)),
		}, nil
	case EntityMrFile, EntityCi, EntityVuln:
		return []*qdrant.Condition{qdrant.NewMatch("kind", string(e))}, nil
	case EntityUser, EntityNote, EntityWorkItem, EntityMilestone, EntityLabel, EntityRunner:
		// Orbit families carry their own kind discriminator; no node_type.
		return []*qdrant.Condition{qdrant.NewMatch("kind", string(e))}, nil
	}
	return nil, fmt.Errorf("graph: invalid start entity %q (want definition|file|mr_file|ci|vuln|user|note|work_item|milestone|label|runner)", e)
}

// typedConditions maps the typed Filter fields to Qdrant match conditions.
func typedConditions(f Filter) []*qdrant.Condition {
	return fieldsConditions(map[string]string{
		"repo":      f.Repo,
		"group_org": f.GroupOrg,
		"vcs_type":  f.VCSType,
		"branch":    f.Branch,
		"file_path": f.FilePath,
		"package":   f.Package,
		"symbol":    f.Symbol,
	})
}

// scopeConditions keeps only the fields every point kind carries (nodes,
// edges, mr_file, ci, vuln): those are safe to require on an edge scan, so a
// hop can never leave the repository/branch an instance serves (SC-007).
func scopeConditions(f Filter) []*qdrant.Condition {
	return fieldsConditions(map[string]string{
		"repo":      f.Repo,
		"group_org": f.GroupOrg,
		"vcs_type":  f.VCSType,
		"branch":    f.Branch,
	})
}

// fieldsConditions emits one match per non-empty field, in a fixed field order
// so compiled filters are deterministic.
func fieldsConditions(fields map[string]string) []*qdrant.Condition {
	var out []*qdrant.Condition
	for _, field := range []string{"repo", "group_org", "vcs_type", "branch", "file_path", "package", "symbol"} {
		if fields[field] != "" {
			out = append(out, qdrant.NewMatch(field, fields[field]))
		}
	}
	return out
}

// selective reports whether a Filter is guaranteed to bound the scan: a typed
// selective field, or a selective field inside a raw must chain.
func selective(f Filter) bool {
	if f.Repo != "" || f.FilePath != "" || f.Symbol != "" {
		return true
	}
	return conditionsSelective(f.Conditions)
}

// conditionsSelective walks must chains only: a selective predicate under
// should (OR) or must_not cannot bound the result set.
func conditionsSelective(conds []*qdrant.Condition) bool {
	for _, c := range conds {
		if selectiveFields[c.GetField().GetKey()] {
			return true
		}
		if nf := c.GetFilter(); nf != nil && conditionsSelective(nf.GetMust()) {
			return true
		}
	}
	return false
}

// hasEmptyMatchAny reports whether a must chain contains an empty MatchAny
// list (keywords or integers). Such a condition can never be satisfied, so the
// query matches nothing and no request is needed. It mirrors the store's
// short-circuit so the compiler and the scanner agree; empty lists under
// should/must_not keep Qdrant semantics and are not treated as matches-nothing.
func hasEmptyMatchAny(f *qdrant.Filter) bool {
	for _, c := range f.GetMust() {
		if fc := c.GetField(); fc != nil {
			if kw := fc.GetMatch().GetKeywords(); kw != nil && len(kw.GetStrings()) == 0 {
				return true
			}
			if in := fc.GetMatch().GetIntegers(); in != nil && len(in.GetIntegers()) == 0 {
				return true
			}
		}
		if nf := c.GetFilter(); nf != nil && hasEmptyMatchAny(nf) {
			return true
		}
	}
	return false
}
