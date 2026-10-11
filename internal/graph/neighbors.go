package graph

import (
	"context"
	"fmt"
	"sort"

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
		if len(pts) > maxEdgesPerHop {
			pts, truncated = pts[:maxEdgesPerHop], true
		}
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
	for _, e := range edges {
		m := e.GetPayload()
		from, to := payloadString(m, "from_key"), payloadString(m, "to_key")
		et := payloadString(m, "edge_type")
		if from == q.NodeKey {
			note(to, payloadString(m, "to_type"), et)
		}
		if to == q.NodeKey {
			note(from, payloadString(m, "from_type"), et)
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
			if _, ok := resolved[nk]; !ok {
				resolved[nk] = n // a file's first definition node represents the file
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

// neighborEdgeFilter is the 1-hop edge filter of one direction: the Compile
// edge shape (kind + scope + raw conditions) plus rel_types/edge_type and the
// centre endpoint.
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
	field := "from_key"
	if dir == Incoming {
		field = "to_key"
	}
	f.Must = append(f.Must, qdrant.NewMatch(field, q.NodeKey))
	return f
}
