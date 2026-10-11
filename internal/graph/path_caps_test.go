package graph

import (
	"context"
	"testing"

	"github.com/qdrant/go-client/qdrant"
)

// capsScroller is a minimal Scroller for the depth-clamp check: it serves the
// fixture edges whose canonical endpoints match the hop filter — the outgoing
// from_key / incoming to_key frontier — and the edge_type condition. Legacy
// spellings are not part of the fixture.
type capsScroller struct{ edges []*qdrant.RetrievedPoint }

func (s *capsScroller) ScrollAll(_ context.Context, f *qdrant.Filter, _ uint32) ([]*qdrant.RetrievedPoint, error) {
	from := capsFilterValues(f, "from_key")
	to := capsFilterValues(f, "to_key")
	rel := capsFilterValues(f, "edge_type")
	var out []*qdrant.RetrievedPoint
	for _, e := range s.edges {
		m := e.GetPayload()
		if len(rel) > 0 && !capsContains(rel, m["edge_type"].GetStringValue()) {
			continue
		}
		fk, tk := m["from_key"].GetStringValue(), m["to_key"].GetStringValue()
		if (len(from) > 0 && capsContains(from, fk)) || (len(to) > 0 && capsContains(to, tk)) {
			out = append(out, e)
		}
	}
	return out, nil
}

// capsFilterValues extracts the keyword values of every condition on field:
// the direct must matchers plus the endpoint OR branches nested under a must
// filter condition (the pathHopFilter shape).
func capsFilterValues(f *qdrant.Filter, field string) []string {
	var out []string
	add := func(c *qdrant.Condition) {
		fc := c.GetField()
		if fc == nil || fc.GetKey() != field {
			return
		}
		if kw := fc.GetMatch().GetKeyword(); kw != "" {
			out = append(out, kw)
		}
		out = append(out, fc.GetMatch().GetKeywords().GetStrings()...)
	}
	for _, c := range f.GetMust() {
		add(c)
		if nf := c.GetFilter(); nf != nil {
			for _, sc := range nf.GetShould() {
				add(sc)
			}
		}
	}
	return out
}

func capsContains(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}

// capsChain is the import chain a.go→b.go→c.go→d.go of repo r: three hops.
func capsChain() (*capsScroller, string, string) {
	a, b, c, d := RefFile("r", "a.go"), RefFile("r", "b.go"), RefFile("r", "c.go"), RefFile("r", "d.go")
	edge := func(id, from, to string) *qdrant.RetrievedPoint {
		return &qdrant.RetrievedPoint{Id: qdrant.NewID(id), Payload: qdrant.NewValueMap(map[string]any{
			"kind": "edge", "edge_type": "imports", "from_key": from, "to_key": to,
			"from_type": "file", "to_type": "file",
		})}
	}
	return &capsScroller{edges: []*qdrant.RetrievedPoint{
		edge("e1", a, b), edge("e2", b, c), edge("e3", c, d),
	}}, a, d
}

// TestPathDepthCaps — FR-025/FR-026: the effective shortest-path depth is
// min(compiled hops, MCP_GRAPH_PATH_MAX_DEPTH). The caps state restores the
// contract-bound default for a non-positive setting, clamps a setting above the
// bound (≤3) and keeps an in-range setting; behaviourally the cap narrows a
// deeper compiled query, the compiled hops still bound a broader cap, and the
// cap is a depth bound, not a kill switch.
func TestPathDepthCaps(t *testing.T) {
	defer SetQueryCaps(0, 0) // restore the default for the other tests

	for _, tc := range []struct{ set, want int }{{0, 3}, {-4, 3}, {9, 3}, {2, 2}, {3, 3}, {1, 1}} {
		SetQueryCaps(tc.set, 0)
		if queryCaps.pathMaxDepth != tc.want {
			t.Errorf("SetQueryCaps(%d): pathMaxDepth = %d, want %d", tc.set, queryCaps.pathMaxDepth, tc.want)
		}
	}

	sc, a, d := capsChain()
	q := func(hops int) Query {
		return Query{
			Mode:     ModePathFinding,
			FromKey:  a,
			ToKey:    d,
			RelTypes: []string{"imports"},
			Hops:     hops,
			Filter:   Filter{Repo: "r"},
		}
	}
	for _, tc := range []struct {
		name  string
		cap   int
		hops  int
		found bool
	}{
		{"default cap, compiled 3", 0, 3, true},
		{"cap 3, compiled 3", 3, 3, true},
		{"cap clamped from 9, compiled 3", 9, 3, true},
		{"cap 2 narrows compiled 3", 2, 3, false},
		{"cap 1 narrows compiled 3", 1, 3, false},
		{"compiled 2 bounds cap 3", 3, 2, false},
	} {
		SetQueryCaps(tc.cap, 0)
		res, err := ShortestPath(context.Background(), sc, q(tc.hops), Coverage{Repo: "r"})
		if err != nil {
			t.Fatalf("%s: ShortestPath: %v", tc.name, err)
		}
		if got := res.Status == PathFound; got != tc.found {
			t.Errorf("%s: status = %q, want found=%v", tc.name, res.Status, tc.found)
		}
		if tc.found && (len(res.Nodes) != 4 || len(res.Edges) != 3) {
			t.Errorf("%s: nodes=%v edges=%d, want the full 3-hop chain", tc.name, res.Nodes, len(res.Edges))
		}
	}

	// A cap of 1 still finds a one-hop chain: the cap narrows the depth, it
	// does not disable the search.
	SetQueryCaps(1, 0)
	res, err := ShortestPath(context.Background(), sc, Query{
		Mode: ModePathFinding, FromKey: a, ToKey: RefFile("r", "b.go"),
		RelTypes: []string{"imports"}, Hops: 3, Filter: Filter{Repo: "r"},
	}, Coverage{Repo: "r"})
	if err != nil || res.Status != PathFound || len(res.Edges) != 1 {
		t.Fatalf("cap 1 over a one-hop chain = %+v err=%v, want found with 1 hop", res, err)
	}
}
