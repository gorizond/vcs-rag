package graph_test

import (
	"context"
	"testing"

	"github.com/qdrant/go-client/qdrant"

	"github.com/gorizond/vcs-rag/internal/graph"
)

// T049: the orbit edge families carry only canonical endpoints (from_key/to_key
// on edges, node_key on nodes). The legacy from_file/to_package fallback for
// code edges stays pinned by TestTraverseDedupesAndCaps and
// TestTraverseIncomingEndpoints (dsl_test.go); these tests pin the canonical
// branch and the mixed frontier.
func TestTraverseCanonicalOrbitKeys(t *testing.T) {
	wi := graph.RefWorkItem("r", "issue", "7")
	author := graph.RefUser("gitlab.com", "42")
	start := []*qdrant.RetrievedPoint{
		point("11111111-1111-5111-8111-111111111111", "work_item",
			map[string]string{"repo": "r", "node_key": wi}),
	}
	edges := []*qdrant.RetrievedPoint{
		point("22222222-2222-5222-8222-222222222222", "edge",
			map[string]string{"edge_type": "authored", "from_key": wi, "to_key": author}),
	}
	sc := &fakeScroller{results: [][]*qdrant.RetrievedPoint{start, edges}}
	q := graph.Query{
		StartEntity: graph.EntityWorkItem,
		Start:       graph.Filter{Repo: "r"},
		EdgeType:    graph.EdgeAuthored,
		Direction:   graph.Outgoing,
		Hops:        2,
		Limit:       10,
	}
	res, err := graph.Traverse(context.Background(), sc, q, graph.Coverage{Repo: "r"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Points) != 2 || res.Truncated {
		t.Fatalf("points=%d truncated=%v", len(res.Points), res.Truncated)
	}
	if len(sc.calls) != 3 {
		t.Fatalf("expected start + two hop scans, got %d", len(sc.calls))
	}
	if !hasEndpointKey(sc.calls[1], "from_key", wi) {
		t.Fatalf("outgoing hop misses the canonical from_key: %v", sc.calls[1])
	}
	// Hop 2 derives its frontier from the edge's from_key; without the
	// canonical branch the walk would stop after one hop (the T049 bug).
	if !hasEndpointKey(sc.calls[2], "from_key", wi) {
		t.Fatalf("hop 2 misses the canonical from_key: %v", sc.calls[2])
	}
}

func TestTraverseCanonicalIncomingKeys(t *testing.T) {
	author := graph.RefUser("gitlab.com", "42")
	start := []*qdrant.RetrievedPoint{
		point("33333333-3333-5333-8333-333333333333", "user",
			map[string]string{"repo": "r", "node_key": author}),
	}
	sc := &fakeScroller{results: [][]*qdrant.RetrievedPoint{start}}
	q := graph.Query{
		StartEntity: graph.EntityUser,
		Start:       graph.Filter{Repo: "r"},
		EdgeType:    graph.EdgeAuthored,
		Direction:   graph.Incoming,
		Hops:        1,
		Limit:       10,
	}
	if _, err := graph.Traverse(context.Background(), sc, q, graph.Coverage{}); err != nil {
		t.Fatal(err)
	}
	if len(sc.calls) != 2 {
		t.Fatalf("expected start + one hop scan, got %d", len(sc.calls))
	}
	hop := sc.calls[1]
	if !hasEndpointKey(hop, "to_key", author) {
		t.Fatalf("incoming hop misses the canonical to_key: %v", hop)
	}
	// A canonical-only point must not leak into the legacy families.
	if hasEndpointKey(hop, "to_symbol", author) {
		t.Fatalf("canonical key leaked into the symbol endpoint: %v", hop)
	}
}

// TestTraverseCanonicalLegacyCombine — a file node carries both spellings, so
// the outgoing hop ORs canonical from_key (new edges) and legacy from_file
// (old edges) without widening to the package (US5).
func TestTraverseCanonicalLegacyCombine(t *testing.T) {
	ref := graph.RefFile("r", "a.go")
	start := []*qdrant.RetrievedPoint{
		point("44444444-4444-5444-8444-444444444444", "node",
			map[string]string{"repo": "r", "node_key": ref, "package": "a", "file_path": "a.go", "symbol": "F"}),
	}
	sc := &fakeScroller{results: [][]*qdrant.RetrievedPoint{start}}
	q := graph.Query{
		StartEntity: graph.EntityFile,
		Start:       graph.Filter{Repo: "r", FilePath: "a.go"},
		EdgeType:    graph.EdgeImports,
		Direction:   graph.Outgoing,
		Hops:        1,
		Limit:       10,
	}
	if _, err := graph.Traverse(context.Background(), sc, q, graph.Coverage{}); err != nil {
		t.Fatal(err)
	}
	hop := sc.calls[1]
	if !hasEndpointKey(hop, "from_key", ref) || !hasEndpointKey(hop, "from_file", "a.go") {
		t.Fatalf("outgoing hop must OR canonical and legacy file endpoints: %v", hop)
	}
	if hasEndpointKey(hop, "from_package", graph.PkgKey("r", "a")) {
		t.Fatalf("outgoing hop widened to the package: %v", hop)
	}
}
