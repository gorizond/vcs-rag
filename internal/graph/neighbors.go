package graph

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/qdrant/go-client/qdrant"
)

// DirectionBoth is the neighbors-only expansion direction (outgoing+incoming).
// It is not a legal traversal Direction (Compile rejects it), so it lives next
// to the engine that honours it.
const DirectionBoth Direction = "both"

// Neighbor is one 1-hop peer of the centre. Resolved peers carry their stored
// node point; unresolved package/file/symbol peers are pseudo-nodes
// (Resolved=false) synthesized from the edge fields. EdgeTypes are the
// relation labels linking the peer to the centre.
type Neighbor struct {
	Key       string
	Type      string
	Resolved  bool
	Node      *qdrant.RetrievedPoint
	EdgeTypes []string
}

// NeighborResult is the neighbours answer; Truncated reports an edge-scan or
// peer-list cap cut.
type NeighborResult struct {
	Neighbors []Neighbor
	Truncated bool
}

// Neighbors returns the 1-hop neighbours of q.NodeKey (T014, plan R4):
// direction outgoing (from_key=centre), incoming (to_key=centre) or both;
// rel_types restricts edges (empty = all). Peers are resolved to stored nodes
// by node_key; unresolved peers become pseudo-nodes. The neighbour list is
// shaped by the query limit (default 30, max 1000); a cut sets Truncated.
//
// Each direction's edge filter ORs the canonical key with the legacy spelling
// derived from it (T051, mirroring the traversal fallback of T049), so edge
// points written before from_key/to_key existed are still reachable. The
// legacy branch matches only when the canonical key is absent, so a canonical
// edge is never re-interpreted through its legacy fields.
//
// Unlike traversal, no Compile runs here: direction "both" is outside the
// traversal Direction enum, and the bounded centre node_key is the selective
// predicate of this mode. Scope fields (repo/branch/...) still ride on the
// edge filter exactly as Compile builds them, so a hop cannot leave the
// instance scope.
func Neighbors(ctx context.Context, sc Scroller, q Query) (NeighborResult, error) {
	res := NeighborResult{Neighbors: []Neighbor{}}
	if q.NodeKey == "" {
		return NeighborResult{}, fmt.Errorf("graph: neighbors requires node_key")
	}
	for _, rt := range q.RelTypes {
		if !knownEdgeType(EdgeType(rt)) {
			return NeighborResult{}, fmt.Errorf("graph: neighbors: invalid rel_type %q", rt)
		}
	}
	dir := q.Direction
	if dir == "" {
		dir = Outgoing
	}
	switch dir {
	case Outgoing, Incoming, DirectionBoth:
	default:
		return NeighborResult{}, fmt.Errorf("graph: neighbors: invalid direction %q (want outgoing|incoming|both)", q.Direction)
	}
	if sc == nil {
		return res, nil
	}
	limit := q.Limit
	if limit <= 0 {
		limit = defaultLimit
	} else if limit > maxLimit {
		limit = maxLimit
	}

	dirs := []Direction{dir}
	if dir == DirectionBoth {
		dirs = []Direction{Outgoing, Incoming}
	}
	edges := map[string]*qdrant.RetrievedPoint{}
	truncated := false
	for _, d := range dirs {
		flt := neighborEdgeFilter(q, d)
		if hasEmptyMatchAny(flt) {
			continue
		}
		pts, err := sc.ScrollAll(ctx, flt, graphPageSize)
		if err != nil {
			return NeighborResult{}, fmt.Errorf("graph: neighbors %s scan: %w", d, err)
		}
		// The centre is one node: its edge scan is the per-node edge cap
		// (FR-025, MCP_GRAPH_MAX_EDGES_PER_NODE); a cut is an honest truncation.
		pts, cut := edgeCap(pts)
		truncated = truncated || cut
		for _, p := range pts {
			edges[pointIDKey(p.GetId())] = p
		}
	}

	type peer struct {
		key, typ  string
		edgeTypes map[string]bool
	}
	peers := map[string]*peer{}
	note := func(key, typ, et string) {
		if key == "" {
			return
		}
		p, ok := peers[key]
		if !ok {
			p = &peer{key: key, edgeTypes: map[string]bool{}}
			peers[key] = p
		}
		if typ != "" {
			p.typ = typ
		}
		if et != "" {
			p.edgeTypes[et] = true
		}
	}
	lc := legacyCentre(q.NodeKey)
	for _, e := range edges {
		m := e.GetPayload()
		et := payloadString(m, "edge_type")
		from, to := payloadString(m, "from_key"), payloadString(m, "to_key")
		if from == q.NodeKey || (from == "" && lc.file != "" && payloadString(m, "from_file") == lc.file) {
			key, typ := to, payloadString(m, "to_type")
			if key == "" {
				key, typ = legacyTarget(m, typ)
			}
			note(key, typ, et)
		}
		if to == q.NodeKey || (to == "" && legacyTargetHit(m, lc)) {
			key, typ := from, payloadString(m, "from_type")
			if key == "" {
				key, typ = payloadString(m, "from_file"), legacyType(typ, "file")
			}
			note(key, typ, et)
		}
	}
	keys := make([]string, 0, len(peers))
	for k := range peers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > limit {
		keys, truncated = keys[:limit], true
	}

	// Resolve peers to nodes by node_key. Deliberately unscoped: canonical
	// keys already pin repo/host, and user/runner nodes carry no repo.
	resolved := map[string]*qdrant.RetrievedPoint{}
	if len(keys) > 0 {
		flt := &qdrant.Filter{Must: []*qdrant.Condition{qdrant.NewMatchKeywords("node_key", keys...)}}
		nodes, err := sc.ScrollAll(ctx, flt, graphPageSize)
		if err != nil {
			return NeighborResult{}, fmt.Errorf("graph: neighbors resolve: %w", err)
		}
		for _, n := range nodes {
			nk := payloadString(n.GetPayload(), "node_key")
			cur, ok := resolved[nk]
			if !ok {
				resolved[nk] = n
				continue
			}
			// A file ref is carried by both the code-graph definition nodes
			// and the schema-v3 file node: prefer the definition — a
			// deterministic pick that mirrors the pre-v3 shape where a file
			// was only its definition nodes (the scan order is not stable).
			if isDefinition(n) && !isDefinition(cur) {
				resolved[nk] = n
			}
		}
	}

	out := make([]Neighbor, 0, len(keys))
	for _, k := range keys {
		p := peers[k]
		nb := Neighbor{Key: k, Type: p.typ}
		for et := range p.edgeTypes {
			nb.EdgeTypes = append(nb.EdgeTypes, et)
		}
		sort.Strings(nb.EdgeTypes)
		if n, ok := resolved[k]; ok {
			nb.Resolved, nb.Node = true, n
		}
		out = append(out, nb)
	}
	res.Neighbors, res.Truncated = out, truncated
	return res, nil
}

// isDefinition reports whether a resolved point is a code-graph definition
// node (kind=node, node_type=definition) — the preferred representative of a
// file ref when both it and a schema-v3 file node carry the same node_key.
func isDefinition(p *qdrant.RetrievedPoint) bool {
	m := p.GetPayload()
	return payloadString(m, "kind") == string(KindNode) &&
		payloadString(m, "node_type") == string(NodeDefinition)
}

// neighborEdgeFilter is the 1-hop edge filter of one direction: the Compile
// edge shape (kind + scope + raw conditions) plus rel_types/edge_type and the
// centre endpoint. Like the traversal hop filter (T049), the endpoint is an OR
// of the canonical key and the legacy spellings derived from it (T051): an
// empty legacy spelling drops its branch, so a centre ref with no pre-T007
// twin (user/note/...) keeps a canonical-only filter.
func neighborEdgeFilter(q Query, dir Direction) *qdrant.Filter {
	f := &qdrant.Filter{}
	f.Must = append(f.Must, qdrant.NewMatch("kind", string(KindEdge)))
	if len(q.RelTypes) > 0 {
		f.Must = append(f.Must, qdrant.NewMatchKeywords("edge_type", q.RelTypes...))
	} else if q.EdgeType != "" && q.EdgeType != "*" {
		f.Must = append(f.Must, qdrant.NewMatch("edge_type", string(q.EdgeType)))
	}
	f.Must = append(f.Must, scopeConditions(q.Start)...)
	f.Must = append(f.Must, scopeConditions(q.Filter)...)
	f.Must = append(f.Must, q.Filter.Conditions...)
	lc := legacyCentre(q.NodeKey)
	var pairs []endpointKeys
	if dir == Incoming {
		pairs = []endpointKeys{
			{"to_key", []string{q.NodeKey}},
			{"to_package", keyList(lc.pkgKey)},
			{"to_package_name", keyList(lc.pkgName)},
			{"to_symbol", keyList(lc.sym)},
		}
	} else {
		pairs = []endpointKeys{
			{"from_key", []string{q.NodeKey}},
			{"from_file", keyList(lc.file)},
		}
	}
	if cond := endpointOr(pairs...); cond != nil {
		f.Must = append(f.Must, cond)
	}
	return f
}

// centreLegacy is the pre-T007 endpoint spelling of a canonical centre ref,
// derived from the ref itself (no extra scan): the file path of a file ref
// (from_file), the repo-scoped PkgKey and bare name of a package ref
// (to_package/to_package_name), the symbol of a def ref (to_symbol). Every
// field stays empty for a ref with no legacy twin.
type centreLegacy struct{ file, pkgKey, pkgName, sym string }

// legacyCentre parses the canonical ref spellings ids.go builds:
// repo|file:<path>, repo|pkg:<path> and repo|def:<pkg>.<symbol>.
func legacyCentre(ref string) centreLegacy {
	repo, rest := "", ref
	if i := strings.Index(ref, "|"); i >= 0 {
		repo, rest = ref[:i], ref[i+1:]
	}
	switch {
	case strings.HasPrefix(rest, "file:"):
		return centreLegacy{file: rest[len("file:"):]}
	case strings.HasPrefix(rest, "pkg:"):
		p := rest[len("pkg:"):]
		return centreLegacy{pkgKey: PkgKey(repo, p), pkgName: PackageName(p)}
	case strings.HasPrefix(rest, "def:"):
		d := rest[len("def:"):]
		if i := strings.LastIndex(d, "."); i >= 0 {
			return centreLegacy{sym: d[i+1:]}
		}
	}
	return centreLegacy{}
}

// keyList wraps a derived legacy key: nil when empty, so endpointOr drops the
// branch instead of emitting a matches-nothing MatchAny.
func keyList(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}

// legacyTarget picks the legacy target endpoint of a pre-parity edge — the
// stored spelling in its old shape (imports: to_package first, then
// to_package_name; defines: to_symbol) — and the type the field itself implies
// when to_type is absent (edges written before canonical keys).
func legacyTarget(m map[string]*qdrant.Value, typ string) (string, string) {
	if v := payloadString(m, "to_package"); v != "" {
		return v, legacyType(typ, "package")
	}
	if v := payloadString(m, "to_package_name"); v != "" {
		return v, legacyType(typ, "package")
	}
	if v := payloadString(m, "to_symbol"); v != "" {
		return v, legacyType(typ, "symbol")
	}
	return "", typ
}

// legacyTargetHit reports whether a pre-parity edge's target endpoint matches
// the centre's legacy spelling.
func legacyTargetHit(m map[string]*qdrant.Value, lc centreLegacy) bool {
	return (lc.pkgKey != "" && payloadString(m, "to_package") == lc.pkgKey) ||
		(lc.pkgName != "" && payloadString(m, "to_package_name") == lc.pkgName) ||
		(lc.sym != "" && payloadString(m, "to_symbol") == lc.sym)
}

// legacyType keeps a stored endpoint type, else falls back to the one the
// legacy field implies.
func legacyType(typ, fallback string) string {
	if typ != "" {
		return typ
	}
	return fallback
}
