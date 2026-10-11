package mcp

import (
	"context"
	"testing"

	"github.com/qdrant/go-client/qdrant"
)

// fakeAggStore — fakeGraphReader + the exact-count surface of the aggregation
// adapter (Count/Facet).
type fakeAggStore struct {
	*fakeGraphReader
	facetKey string
	facet    map[string]uint64
	count    uint64
}

func (f *fakeAggStore) Count(context.Context, *qdrant.Filter) (uint64, error) { return f.count, nil }

func (f *fakeAggStore) Facet(_ context.Context, key string, _ *qdrant.Filter, _ uint64) (map[string]uint64, error) {
	f.facetKey = key
	return f.facet, nil
}

// TestZZQueryGraphModesSmoke — transient check of the T016 dispatch: each mode
// reaches its engine and returns its response shape.
func TestZZQueryGraphModesSmoke(t *testing.T) {
	ctx := context.Background()

	// aggregation: grouped count goes through the adapter's Facet.
	agg := &fakeAggStore{fakeGraphReader: &fakeGraphReader{}, facet: map[string]uint64{"acme/demo": 3}, count: 3}
	tl := &tools{embedder: &mockEmbedder{}, searcher: &mockSearcher{}, graph: agg}
	_, out, err := tl.queryGraph(ctx, nil, queryGraphInput{
		Mode: "aggregation", Op: "count", GroupBy: "repo",
		Start: queryGraphStart{Filters: map[string]any{"repo": "acme/demo"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ar, ok := out.(aggregationResponse)
	if !ok || agg.facetKey != "repo" || len(ar.Groups) != 1 || ar.Groups[0].Key != "acme/demo" || ar.Groups[0].Count != 3 {
		t.Fatalf("aggregation = %T %+v (facetKey %q)", out, out, agg.facetKey)
	}

	// aggregation non-selective: refused before any scan.
	scans := len(agg.filters)
	_, out, err = tl.queryGraph(ctx, nil, queryGraphInput{Mode: "aggregation", Op: "count"})
	if err != nil {
		t.Fatal(err)
	}
	if te, ok := out.(toolError); !ok || te.Code != codeNonSelective || len(agg.filters) != scans {
		t.Fatalf("non-selective aggregation = %T %+v scans=%d", out, out, len(agg.filters))
	}

	a, b, c := "r|file:a.go", "r|file:b.go", "r|file:c.go"
	eAB := &qdrant.RetrievedPoint{Id: qdrant.NewID("11111111-1111-5111-8111-111111111111"), Payload: qdrant.NewValueMap(map[string]any{
		"kind": "edge", "edge_type": "imports", "from_key": a, "to_key": b, "from_type": "file", "to_type": "package",
	})}
	eBC := &qdrant.RetrievedPoint{Id: qdrant.NewID("22222222-2222-5222-8222-222222222222"), Payload: qdrant.NewValueMap(map[string]any{
		"kind": "edge", "edge_type": "imports", "from_key": b, "to_key": c, "from_type": "package", "to_type": "file",
	})}
	eBA := &qdrant.RetrievedPoint{Id: qdrant.NewID("33333333-3333-5333-8333-333333333333"), Payload: qdrant.NewValueMap(map[string]any{
		"kind": "edge", "edge_type": "imports", "from_key": b, "to_key": a, "from_type": "package", "to_type": "file",
	})}
	nodeB := &qdrant.RetrievedPoint{Id: qdrant.NewID("44444444-4444-5444-8444-444444444444"), Payload: qdrant.NewValueMap(map[string]any{
		"kind": "node", "node_key": b, "symbol": "F", "repo": "r",
	})}

	// neighbors: 1-hop peers of the centre, resolved by node_key.
	tl.graph = &fakeGraphReader{points: []*qdrant.RetrievedPoint{eAB, eBA, nodeB}}
	_, out, err = tl.queryGraph(ctx, nil, queryGraphInput{
		Mode: "neighbors", NodeKey: a, Direction: "both", RelTypes: []string{"imports"},
	})
	if err != nil {
		t.Fatal(err)
	}
	nr, ok := out.(neighborsResponse)
	if !ok || len(nr.Neighbors) != 1 || nr.Neighbors[0].Key != b || !nr.Neighbors[0].Resolved || nr.Neighbors[0].Node["symbol"] != "F" {
		t.Fatalf("neighbors = %T %+v", out, out)
	}

	// path_finding: a→b→c is found across two expansions.
	tl.graph = &scriptedGraphReader{responses: [][]*qdrant.RetrievedPoint{{eAB}, {eBC}}}
	_, out, err = tl.queryGraph(ctx, nil, queryGraphInput{
		Mode: "path_finding", From: a, To: c, Type: "shortest", MaxDepth: 3, RelTypes: []string{"imports"},
	})
	if err != nil {
		t.Fatal(err)
	}
	pr, ok := out.(pathResponse)
	if !ok || pr.Status != "found" || len(pr.Path) != 3 || len(pr.Edges) != 2 {
		t.Fatalf("path = %T %+v", out, out)
	}

	// Mode-argument validation: neighbors without a centre, path without
	// rel_types — invalid_input before any engine call.
	for name, in := range map[string]queryGraphInput{
		"neighbors without node_key": {Mode: "neighbors"},
		"path without rel_types":     {Mode: "path_finding", From: a, To: c},
		"path without endpoints":     {Mode: "path_finding", RelTypes: []string{"imports"}},
		"bad mode":                   {Mode: "scan"},
	} {
		_, o, e := tl.queryGraph(ctx, nil, in)
		if e != nil {
			t.Fatalf("%s: %v", name, e)
		}
		if te, ok := o.(toolError); !ok || te.Code != codeInvalidInput {
			t.Errorf("%s = %T %+v, want %s", name, o, o, codeInvalidInput)
		}
	}
}
