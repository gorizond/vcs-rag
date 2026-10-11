package graph

import (
	"context"
	"fmt"

	"github.com/qdrant/go-client/qdrant"
)

// PathStatus is the honest answer of a shortest-path search
// (contracts/mcp-tools.md): a truncated or coverage-gapped search must report
// inconclusive instead of claiming the path does not exist.
type PathStatus string

const (
	PathFound        PathStatus = "found"
	PathNotFound     PathStatus = "not_found"
	PathInconclusive PathStatus = "inconclusive"
)

// PathEdge is one hop of a found path, in stored edge direction.
type PathEdge struct {
	FromKey  string
	ToKey    string
	EdgeType string
}

// PathResult is the shortest-path answer. Nodes are the canonical keys from
// FromKey to ToKey (found only); Edges one per hop. Truncated reports that a
// frontier/edge cap cut the search (coverage gaps are visible on Coverage).
type PathResult struct {
	Status    PathStatus
	Nodes     []string
	Edges     []PathEdge
	Truncated bool
	Coverage  Coverage
}

// ShortestPath finds the shortest undirected path between two canonical keys
// (T013, plan R3) with a bidirectional BFS over rel_types edges:
//
//   - the search follows stored edge direction: the forward side expands
//     from_key=frontier, the backward side to_key=frontier; endpoints bounded
//     by canonical keys, depth by the compiled hops (≤3), rel_types required;
//   - the smaller frontier is expanded each step and the intersection of the
//     two visited sets is checked after every expansion — the first meeting
//     yields the minimum number of hops;
//   - a frontier/edge cap, or a Truncated coverage for the repo, makes a
//     missing path inconclusive, never not_found.
//
// Compile validates the query first (selectivity + rel_types); MatchNone
// short-circuits to not_found without touching the Scroller.
func ShortestPath(ctx context.Context, sc Scroller, q Query, coverage Coverage) (PathResult, error) {
	res := PathResult{Status: PathNotFound, Nodes: []string{}, Edges: []PathEdge{}, Coverage: coverage}
	if q.FromKey == "" || q.ToKey == "" {
		return PathResult{}, fmt.Errorf("graph: path_finding requires both from and to keys")
	}
	if len(q.RelTypes) == 0 {
		return PathResult{}, ErrRelTypesRequired
	}
	for _, rt := range q.RelTypes {
		if !knownEdgeType(EdgeType(rt)) {
			return PathResult{}, fmt.Errorf("graph: path_finding: invalid rel_type %q", rt)
		}
	}
	c, err := Compile(q)
	if err != nil {
		return PathResult{}, err
	}
	if c.MatchNone || sc == nil {
		return res, nil
	}
	if q.FromKey == q.ToKey {
		res.Status, res.Nodes = PathFound, []string{q.FromKey}
		return res, nil
	}

	fwd := map[string]int{q.FromKey: 0}
	bwd := map[string]int{q.ToKey: 0}
	parentF := map[string]string{}
	kindF := map[string]string{}
	parentB := map[string]string{}
	kindB := map[string]string{}
	frontF, frontB := []string{q.FromKey}, []string{q.ToKey}

	truncated := false
	meet, best := "", 0

	// expand grows one side by exactly one hop level and checks the new keys
	// against the other side's visited set, keeping the minimum total depth.
	expand := func(dir Direction, fr []string, self, other map[string]int, parents, kinds map[string]string) ([]string, bool, error) {
		cut := false
		if len(fr) > maxFrontier {
			fr, cut = fr[:maxFrontier], true
		}
		edges, err := sc.ScrollAll(ctx, pathHopFilter(c.Edge, dir, fr, q.RelTypes), graphPageSize)
		if err != nil {
			return nil, cut, fmt.Errorf("graph: path hop (%s): %w", dir, err)
		}
		if len(edges) > maxEdgesPerHop {
			edges, cut = edges[:maxEdgesPerHop], true
		}
		var next []string
		for _, e := range edges {
			m := e.GetPayload()
			from, to := payloadString(m, "from_key"), payloadString(m, "to_key")
			parent, peer := from, to
			if dir == Incoming {
				parent, peer = to, from
			}
			d, ok := self[parent]
			if !ok || peer == "" || d+1 > c.Hops {
				continue
			}
			if _, seen := self[peer]; seen {
				continue
			}
			self[peer] = d + 1
			parents[peer] = parent
			kinds[peer] = payloadString(m, "edge_type")
			next = append(next, peer)
		}
		for _, k := range next {
			if od, ok := other[k]; ok {
				if total := self[k] + od; meet == "" || total < best {
					meet, best = k, total
				}
			}
		}
		return next, cut, nil
	}

	for df, db := 0, 0; df+db < c.Hops && len(frontF) > 0 && len(frontB) > 0 && meet == ""; {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		var (
			next []string
			cut  bool
			err  error
		)
		if len(frontF) <= len(frontB) {
			next, cut, err = expand(Outgoing, frontF, fwd, bwd, parentF, kindF)
			df++
			frontF = next
		} else {
			next, cut, err = expand(Incoming, frontB, bwd, fwd, parentB, kindB)
			db++
			frontB = next
		}
		if err != nil {
			return res, err
		}
		truncated = truncated || cut
	}

	res.Truncated = truncated
	if meet == "" {
		if truncated || coverage.Truncated {
			res.Status = PathInconclusive
		}
		return res, nil
	}

	// Reconstruct: forward chain FromKey..meet, then backward chain meet..ToKey.
	fchain := []string{meet}
	for k := meet; k != q.FromKey; {
		p, ok := parentF[k]
		if !ok {
			return res, fmt.Errorf("graph: path reconstruction lost forward link at %q", k)
		}
		fchain = append(fchain, p)
		k = p
	}
	reverseStrings(fchain)
	nodes := append([]string{}, fchain...)
	edges := make([]PathEdge, 0, c.Hops)
	for i := 1; i < len(fchain); i++ {
		edges = append(edges, PathEdge{FromKey: fchain[i-1], ToKey: fchain[i], EdgeType: kindF[fchain[i]]})
	}
	for k := meet; k != q.ToKey; {
		n, ok := parentB[k]
		if !ok {
			return res, fmt.Errorf("graph: path reconstruction lost backward link at %q", k)
		}
		edges = append(edges, PathEdge{FromKey: k, ToKey: n, EdgeType: kindB[k]})
		nodes = append(nodes, n)
		k = n
	}
	res.Status, res.Nodes, res.Edges = PathFound, nodes, edges
	return res, nil
}

// pathHopFilter narrows the compiled edge filter to one hop of a path search:
// the rel_types list plus the endpoint field the direction traverses.
func pathHopFilter(base *qdrant.Filter, dir Direction, keys, rel []string) *qdrant.Filter {
	f := &qdrant.Filter{}
	f.Must = append(f.Must, base.GetMust()...)
	if len(rel) > 0 {
		f.Must = append(f.Must, qdrant.NewMatchKeywords("edge_type", rel...))
	}
	field := "from_key"
	if dir == Incoming {
		field = "to_key"
	}
	f.Must = append(f.Must, qdrant.NewMatchKeywords(field, keys...))
	return f
}

// reverseStrings reverses s in place.
func reverseStrings(s []string) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}
