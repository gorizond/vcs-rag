package graph_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/qdrant/go-client/qdrant"

	"github.com/gorizond/vcs-rag/internal/graph"
)

// fakeScroller serves canned scan results in call order and records the
// filters it was asked for.
type fakeScroller struct {
	calls   []*qdrant.Filter
	results [][]*qdrant.RetrievedPoint
}

func (f *fakeScroller) ScrollAll(_ context.Context, flt *qdrant.Filter, _ uint32) ([]*qdrant.RetrievedPoint, error) {
	f.calls = append(f.calls, flt)
	if len(f.results) == 0 {
		return nil, nil
	}
	out := f.results[0]
	f.results = f.results[1:]
	return out, nil
}

func point(id, kind string, kv map[string]string) *qdrant.RetrievedPoint {
	payload := map[string]*qdrant.Value{"kind": qdrant.NewValueString(kind)}
	for k, v := range kv {
		payload[k] = qdrant.NewValueString(v)
	}
	return &qdrant.RetrievedPoint{Id: qdrant.NewID(id), Payload: payload}
}

func hasMatch(f *qdrant.Filter, field, value string) bool {
	for _, c := range f.GetMust() {
		if fc := c.GetField(); fc != nil && fc.GetKey() == field && fc.GetMatch().GetKeyword() == value {
			return true
		}
	}
	return false
}

func hasEndpointKey(f *qdrant.Filter, field, value string) bool {
	for _, c := range f.GetMust() {
		nf := c.GetFilter()
		if nf == nil {
			continue
		}
		for _, sc := range nf.GetShould() {
			fc := sc.GetField()
			if fc == nil || fc.GetKey() != field {
				continue
			}
			for _, v := range fc.GetMatch().GetKeywords().GetStrings() {
				if v == value {
					return true
				}
			}
		}
	}
	return false
}

func TestCompileValid(t *testing.T) {
	c, err := graph.Compile(graph.Query{
		StartEntity: graph.EntityMrFile,
		Start:       graph.Filter{Repo: "org/repo", FilePath: "a.go"},
		EdgeType:    graph.EdgeImports,
		Direction:   graph.Outgoing,
		Hops:        2,
		Limit:       30,
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if c.Hops != 2 || c.Limit != 30 || c.Direction != graph.Outgoing {
		t.Fatalf("bounds: hops=%d limit=%d dir=%s", c.Hops, c.Limit, c.Direction)
	}
	if !hasMatch(c.Start, "kind", "mr_file") || !hasMatch(c.Start, "repo", "org/repo") || !hasMatch(c.Start, "file_path", "a.go") {
		t.Errorf("start filter misses entity/conditions: %v", c.Start)
	}
	if !hasMatch(c.Edge, "kind", "edge") || !hasMatch(c.Edge, "edge_type", "imports") {
		t.Errorf("edge filter misses kind/edge_type: %v", c.Edge)
	}
	if !hasMatch(c.Edge, "repo", "org/repo") {
		t.Errorf("edge filter must keep the repo scope: %v", c.Edge)
	}
	if c.MatchNone {
		t.Error("valid query must not be MatchNone")
	}
}

func TestCompileNonSelective(t *testing.T) {
	for _, f := range []graph.Filter{
		{Branch: "main"},
		{GroupOrg: "org", Package: "utils"},
		{Conditions: []*qdrant.Condition{qdrant.NewMatch("status", "failed")}},
	} {
		_, err := graph.Compile(graph.Query{Start: f, Hops: 1})
		if !errors.Is(err, graph.ErrNonSelective) {
			t.Fatalf("filter %+v: want ErrNonSelective, got %v", f, err)
		}
		if !strings.Contains(err.Error(), "non_selective_query") {
			t.Fatalf("error must carry the contractual token: %v", err)
		}
	}
}

func TestCompileSelectiveRawCondition(t *testing.T) {
	// The edge-side selective fields (mr_id, from_file, ...) arrive as raw
	// conditions; nested must chains count, should/must_not do not.
	for _, conds := range [][]*qdrant.Condition{
		{qdrant.NewMatch("mr_id", "42")},
		{qdrant.NewFilterAsCondition(&qdrant.Filter{Must: []*qdrant.Condition{qdrant.NewMatch("from_file", "a.go")}})},
	} {
		if _, err := graph.Compile(graph.Query{Start: graph.Filter{Conditions: conds}, Hops: 1}); err != nil {
			t.Fatalf("raw selective condition rejected: %v", err)
		}
	}
	if _, err := graph.Compile(graph.Query{
		Start: graph.Filter{Conditions: []*qdrant.Condition{qdrant.NewFilterAsCondition(&qdrant.Filter{
			Should: []*qdrant.Condition{qdrant.NewMatch("repo", "r")},
		})}},
		Hops: 1,
	}); !errors.Is(err, graph.ErrNonSelective) {
		t.Fatalf("should-only selective field must not count: %v", err)
	}
}

func TestCompileClamps(t *testing.T) {
	c, err := graph.Compile(graph.Query{Start: graph.Filter{Repo: "r"}, Hops: 9, Limit: 5000})
	if err != nil {
		t.Fatal(err)
	}
	if c.Hops != 3 || c.Limit != 1000 {
		t.Fatalf("upper clamp: hops=%d limit=%d", c.Hops, c.Limit)
	}
	c, err = graph.Compile(graph.Query{Start: graph.Filter{Repo: "r"}})
	if err != nil {
		t.Fatal(err)
	}
	if c.Hops != 1 || c.Limit != 30 {
		t.Fatalf("defaults: hops=%d limit=%d", c.Hops, c.Limit)
	}
}

func TestCompileEmptyMatchAnyShortCircuits(t *testing.T) {
	q := graph.Query{Start: graph.Filter{
		Repo:       "r",
		Conditions: []*qdrant.Condition{qdrant.NewMatchKeywords("file_path")},
	}}
	c, err := graph.Compile(q)
	if err != nil {
		t.Fatal(err)
	}
	if !c.MatchNone {
		t.Fatal("empty MatchAny must set MatchNone")
	}
	sc := &fakeScroller{}
	res, err := graph.Traverse(context.Background(), sc, q, graph.Coverage{Repo: "r"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Points) != 0 || res.Truncated || len(sc.calls) != 0 {
		t.Fatalf("short-circuit: points=%d truncated=%v scans=%d", len(res.Points), res.Truncated, len(sc.calls))
	}
}

func TestTraverseDedupesAndCaps(t *testing.T) {
	e1 := map[string]string{"edge_type": "imports", "from_file": "a.go", "to_package": graph.PkgKey("r", "b")}
	start := []*qdrant.RetrievedPoint{
		point("11111111-1111-5111-8111-111111111111", "node",
			map[string]string{"repo": "r", "package": "a", "file_path": "a.go", "symbol": "F"}),
	}
	edges := []*qdrant.RetrievedPoint{
		point("22222222-2222-5222-8222-222222222222", "edge", e1),
		point("22222222-2222-5222-8222-222222222222", "edge", e1), // duplicate ID
		point("33333333-3333-5333-8333-333333333333", "edge",
			map[string]string{"edge_type": "imports", "from_file": "a.go", "to_package": graph.PkgKey("r", "c")}),
	}
	sc := &fakeScroller{results: [][]*qdrant.RetrievedPoint{start, edges}}
	q := graph.Query{
		StartEntity: graph.EntityDefinition,
		Start:       graph.Filter{Repo: "r", FilePath: "a.go"},
		EdgeType:    graph.EdgeImports,
		Direction:   graph.Outgoing,
		Hops:        1,
		Limit:       10,
	}
	res, err := graph.Traverse(context.Background(), sc, q, graph.Coverage{Repo: "r", IndexedCommit: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Points) != 3 || res.Truncated {
		t.Fatalf("dedupe: points=%d truncated=%v", len(res.Points), res.Truncated)
	}
	if res.Coverage.IndexedCommit != "abc" || res.Coverage.Repo != "r" {
		t.Fatalf("coverage not passed through: %+v", res.Coverage)
	}
	if len(sc.calls) != 2 {
		t.Fatalf("expected start + one hop scan, got %d", len(sc.calls))
	}
	// The hop filter must match the frontier on the outgoing endpoints.
	if !hasEndpointKey(sc.calls[1], "from_file", "a.go") || !hasEndpointKey(sc.calls[1], "from_package", graph.PkgKey("r", "a")) {
		t.Fatalf("hop filter lacks frontier endpoints: %v", sc.calls[1])
	}

	// Result limit cuts and reports truncation.
	q.Limit = 2
	sc = &fakeScroller{results: [][]*qdrant.RetrievedPoint{start, edges}}
	res, err = graph.Traverse(context.Background(), sc, q, graph.Coverage{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Points) != 2 || !res.Truncated {
		t.Fatalf("limit cap: points=%d truncated=%v", len(res.Points), res.Truncated)
	}
}

func TestTraverseIncomingEndpoints(t *testing.T) {
	start := []*qdrant.RetrievedPoint{
		point("11111111-1111-5111-8111-111111111111", "node",
			map[string]string{"repo": "r", "package": "a", "file_path": "a.go", "symbol": "F"}),
	}
	sc := &fakeScroller{results: [][]*qdrant.RetrievedPoint{start}}
	q := graph.Query{Start: graph.Filter{Repo: "r"}, Direction: graph.Incoming, Hops: 1, Limit: 5}
	if _, err := graph.Traverse(context.Background(), sc, q, graph.Coverage{}); err != nil {
		t.Fatal(err)
	}
	if len(sc.calls) != 2 {
		t.Fatalf("expected start + one hop scan, got %d", len(sc.calls))
	}
	// Incoming hops arrive at the frontier: to_package/to_symbol, never from_file.
	if !hasEndpointKey(sc.calls[1], "to_package", graph.PkgKey("r", "a")) || hasEndpointKey(sc.calls[1], "from_file", "a.go") {
		t.Fatalf("incoming hop endpoints wrong: %v", sc.calls[1])
	}
}
