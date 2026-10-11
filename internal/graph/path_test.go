package graph_test

import (
	"context"
	"errors"
	"testing"

	"github.com/qdrant/go-client/qdrant"

	"github.com/gorizond/vcs-rag/internal/graph"
)

// edgeScroller — hermetic Scroller for path/neighbours: it serves edge points
// matching the filter's from_key/to_key endpoint AND the edge_type condition
// (so a test can prove rel_types is enforced), and resolves node_key filters
// against the node fixture. Point IDs are synthetic; pointIDKey is only used
// for dedup.
type edgeScroller struct {
	edges   []*qdrant.RetrievedPoint
	nodes   []*qdrant.RetrievedPoint
	err     error
	calls   int
	filters []*qdrant.Filter
}

func (f *edgeScroller) ScrollAll(_ context.Context, flt *qdrant.Filter, _ uint32) ([]*qdrant.RetrievedPoint, error) {
	f.calls++
	f.filters = append(f.filters, flt)
	if f.err != nil {
		return nil, f.err
	}
	if keys := filterValues(flt, "node_key"); len(keys) > 0 {
		var out []*qdrant.RetrievedPoint
		for _, n := range f.nodes {
			if containsKey(keys, payloadStr(n, "node_key")) {
				out = append(out, n)
			}
		}
		return out, nil
	}
	fromKeys := filterValues(flt, "from_key")
	toKeys := filterValues(flt, "to_key")
	relTypes := filterValues(flt, "edge_type")
	var out []*qdrant.RetrievedPoint
	for _, e := range f.edges {
		if len(relTypes) > 0 && !containsKey(relTypes, payloadStr(e, "edge_type")) {
			continue
		}
		if (len(fromKeys) > 0 && containsKey(fromKeys, payloadStr(e, "from_key"))) ||
			(len(toKeys) > 0 && containsKey(toKeys, payloadStr(e, "to_key"))) {
			out = append(out, e)
		}
	}
	return out, nil
}

// filterValues extracts every keyword value of a must-chain condition on
// field: single Match keywords and MatchAny lists both feed the fake's router.
func filterValues(f *qdrant.Filter, field string) []string {
	var out []string
	for _, c := range f.GetMust() {
		fc := c.GetField()
		if fc == nil || fc.GetKey() != field {
			continue
		}
		if kw := fc.GetMatch().GetKeyword(); kw != "" {
			out = append(out, kw)
		}
		out = append(out, fc.GetMatch().GetKeywords().GetStrings()...)
	}
	return out
}

// containsKey reports membership; nil-safe on both sides.
func containsKey(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}

// payloadStr reads a string payload field of a fixture point.
func payloadStr(p *qdrant.RetrievedPoint, key string) string {
	return p.GetPayload()[key].GetStringValue()
}

// edgePoint — one kind=edge fixture with canonical endpoint keys.
func edgePoint(id, et, from, to, fromType, toType string) *qdrant.RetrievedPoint {
	return &qdrant.RetrievedPoint{Id: qdrant.NewID(id), Payload: qdrant.NewValueMap(map[string]any{
		"kind": "edge", "edge_type": et,
		"from_key": from, "to_key": to, "from_type": fromType, "to_type": toType,
	})}
}

// nodePoint — one kind=node fixture carrying a canonical node_key.
func nodePoint(id, nodeKey, symbol string) *qdrant.RetrievedPoint {
	return &qdrant.RetrievedPoint{Id: qdrant.NewID(id), Payload: qdrant.NewValueMap(map[string]any{
		"kind": "node", "node_type": "definition", "node_key": nodeKey, "symbol": symbol, "package": "a",
	})}
}

// pathQuery — a path_finding Query; the repo filter is the selective
// predicate Compile demands.
func pathQuery(from, to string, hops int) graph.Query {
	return graph.Query{
		Mode:     graph.ModePathFinding,
		FromKey:  from,
		ToKey:    to,
		RelTypes: []string{"imports"},
		Hops:     hops,
		Filter:   graph.Filter{Repo: "r"},
	}
}

// TestShortestPathFound — a→b→c imports chain is found bidirectionally, with
// nodes and edges reconstructed in stored edge direction, and rel_types
// enforced on the edge filter.
func TestShortestPathFound(t *testing.T) {
	a, b, c := graph.RefFile("r", "a.go"), graph.RefFile("r", "b.go"), graph.RefFile("r", "c.go")
	sc := &edgeScroller{edges: []*qdrant.RetrievedPoint{
		edgePoint("e1", "imports", a, b, "file", "package"),
		edgePoint("e2", "imports", b, c, "file", "package"),
	}}
	res, err := graph.ShortestPath(context.Background(), sc, pathQuery(a, c, 3), graph.Coverage{Repo: "r"})
	if err != nil {
		t.Fatalf("ShortestPath: %v", err)
	}
	if res.Status != graph.PathFound {
		t.Fatalf("status = %q, want found", res.Status)
	}
	if len(res.Nodes) != 3 || res.Nodes[0] != a || res.Nodes[1] != b || res.Nodes[2] != c {
		t.Fatalf("nodes = %v, want %s→%s→%s", res.Nodes, a, b, c)
	}
	if len(res.Edges) != 2 || res.Edges[0] != (graph.PathEdge{FromKey: a, ToKey: b, EdgeType: "imports"}) {
		t.Fatalf("edges = %+v, want a→b then b→c", res.Edges)
	}
	// Every hop filter carries the rel_types list, so a foreign relation is
	// never traversed.
	for _, flt := range sc.filters {
		if got := filterValues(flt, "edge_type"); len(got) != 1 || got[0] != "imports" {
			t.Fatalf("edge filter rel_types = %v, want [imports]", got)
		}
	}
}

// TestShortestPathNotFound — a bounded search with no meeting point is a
// conclusive not_found.
func TestShortestPathNotFound(t *testing.T) {
	a, b, c := graph.RefFile("r", "a.go"), graph.RefFile("r", "b.go"), graph.RefFile("r", "c.go")
	sc := &edgeScroller{edges: []*qdrant.RetrievedPoint{
		edgePoint("e1", "imports", a, b, "file", "package"),
	}}
	res, err := graph.ShortestPath(context.Background(), sc, pathQuery(a, c, 3), graph.Coverage{Repo: "r"})
	if err != nil {
		t.Fatalf("ShortestPath: %v", err)
	}
	if res.Status != graph.PathNotFound || res.Truncated {
		t.Fatalf("status = %q truncated=%v, want not_found", res.Status, res.Truncated)
	}
}

// TestShortestPathInconclusiveOnCap — when an edge cap cuts the search, a
// missing path must be inconclusive, never not_found.
func TestShortestPathInconclusiveOnCap(t *testing.T) {
	a, c := graph.RefFile("r", "a.go"), graph.RefFile("r", "z.go")
	edges := make([]*qdrant.RetrievedPoint, 0, 20001)
	for i := 0; i < 20001; i++ { // one over maxEdgesPerHop
		edges = append(edges, edgePoint("e"+itoa(i), "imports", a, graph.RefFile("r", "t"+itoa(i)+".go"), "file", "package"))
	}
	sc := &edgeScroller{edges: edges}
	res, err := graph.ShortestPath(context.Background(), sc, pathQuery(a, c, 3), graph.Coverage{Repo: "r"})
	if err != nil {
		t.Fatalf("ShortestPath: %v", err)
	}
	if !res.Truncated || res.Status != graph.PathInconclusive {
		t.Fatalf("status = %q truncated=%v, want inconclusive on cap", res.Status, res.Truncated)
	}
}

// TestShortestPathInconclusiveOnCoverage — a coverage gap makes a missing
// path inconclusive as well.
func TestShortestPathInconclusiveOnCoverage(t *testing.T) {
	a, c := graph.RefFile("r", "a.go"), graph.RefFile("r", "z.go")
	res, err := graph.ShortestPath(context.Background(), &edgeScroller{}, pathQuery(a, c, 3),
		graph.Coverage{Repo: "r", Truncated: true})
	if err != nil {
		t.Fatalf("ShortestPath: %v", err)
	}
	if res.Status != graph.PathInconclusive {
		t.Fatalf("status = %q, want inconclusive", res.Status)
	}
}

// TestShortestPathMaxDepth — the depth cap is conclusive: a longer chain is
// not_found within max_depth and found at max_depth.
func TestShortestPathMaxDepth(t *testing.T) {
	a, b, c, d := graph.RefFile("r", "a.go"), graph.RefFile("r", "b.go"), graph.RefFile("r", "c.go"), graph.RefFile("r", "d.go")
	edges := []*qdrant.RetrievedPoint{
		edgePoint("e1", "imports", a, b, "file", "package"),
		edgePoint("e2", "imports", b, c, "file", "package"),
		edgePoint("e3", "imports", c, d, "file", "package"),
	}
	res, err := graph.ShortestPath(context.Background(), &edgeScroller{edges: edges}, pathQuery(a, d, 2), graph.Coverage{})
	if err != nil {
		t.Fatalf("ShortestPath(depth=2): %v", err)
	}
	if res.Status != graph.PathNotFound {
		t.Fatalf("status = %q, want not_found within depth 2", res.Status)
	}
	res, err = graph.ShortestPath(context.Background(), &edgeScroller{edges: edges}, pathQuery(a, d, 3), graph.Coverage{})
	if err != nil {
		t.Fatalf("ShortestPath(depth=3): %v", err)
	}
	if res.Status != graph.PathFound || len(res.Nodes) != 4 {
		t.Fatalf("status = %q nodes=%v, want the 3-hop chain", res.Status, res.Nodes)
	}
}

// TestShortestPathValidation — rel_types is required, endpoints are required,
// from==to is a zero-hop path.
func TestShortestPathValidation(t *testing.T) {
	a := graph.RefFile("r", "a.go")
	q := pathQuery(a, graph.RefFile("r", "b.go"), 3)
	q.RelTypes = nil
	if _, err := graph.ShortestPath(context.Background(), &edgeScroller{}, q, graph.Coverage{}); !errors.Is(err, graph.ErrRelTypesRequired) {
		t.Fatalf("err = %v, want ErrRelTypesRequired", err)
	}
	if _, err := graph.ShortestPath(context.Background(), &edgeScroller{}, graph.Query{RelTypes: []string{"imports"}}, graph.Coverage{}); err == nil {
		t.Fatal("missing endpoints must be refused")
	}
	res, err := graph.ShortestPath(context.Background(), &edgeScroller{}, pathQuery(a, a, 3), graph.Coverage{})
	if err != nil || res.Status != graph.PathFound || len(res.Nodes) != 1 || len(res.Edges) != 0 {
		t.Fatalf("from==to = %+v err=%v, want a zero-hop found path", res, err)
	}
}

// itoa is a tiny local integer formatter for fixture IDs.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
