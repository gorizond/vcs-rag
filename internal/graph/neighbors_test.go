package graph_test

import (
	"context"
	"testing"

	"github.com/qdrant/go-client/qdrant"

	"github.com/gorizond/vcs-rag/internal/graph"
)

// neighborFixtures — one centre file with an outgoing import to a resolvable
// package, an outgoing defines to an unresolvable symbol, and an incoming
// authored edge from a user. Only the package has a stored node.
func neighborFixtures() (q graph.Query, sc *edgeScroller) {
	centre := graph.RefFile("r", "a.go")
	pkg := graph.RefPkg("r", "github.com/x/y")
	sym := "r|def:a.F"
	user := "user:gitlab.example#7"
	sc = &edgeScroller{
		edges: []*qdrant.RetrievedPoint{
			edgePoint("e1", "imports", centre, pkg, "file", "package"),
			edgePoint("e2", "defines", centre, sym, "file", "symbol"),
			edgePoint("e3", "authored", user, centre, "user", "file"),
		},
		nodes: []*qdrant.RetrievedPoint{nodePoint("n1", pkg, "F")},
	}
	q = graph.Query{Mode: graph.ModeNeighbors, NodeKey: centre, Filter: graph.Filter{Repo: "r"}}
	return q, sc
}

// neighborByKey indexes the answer for assertions.
func neighborByKey(res graph.NeighborResult, key string) (graph.Neighbor, bool) {
	for _, n := range res.Neighbors {
		if n.Key == key {
			return n, true
		}
	}
	return graph.Neighbor{}, false
}

// TestNeighborsOutgoing — outgoing 1-hop: peers resolve by node_key, and an
// unresolvable peer comes back as a pseudo-node.
func TestNeighborsOutgoing(t *testing.T) {
	q, sc := neighborFixtures()
	q.Direction = graph.Outgoing
	q.RelTypes = []string{"imports", "defines"}
	res, err := graph.Neighbors(context.Background(), sc, q)
	if err != nil {
		t.Fatalf("Neighbors: %v", err)
	}
	if len(res.Neighbors) != 2 {
		t.Fatalf("neighbors = %+v, want 2 outgoing", res.Neighbors)
	}
	pkg, ok := neighborByKey(res, graph.RefPkg("r", "github.com/x/y"))
	if !ok || !pkg.Resolved || pkg.Node == nil || payloadStr(pkg.Node, "symbol") != "F" {
		t.Fatalf("package peer = %+v, want resolved node symbol F", pkg)
	}
	sym, ok := neighborByKey(res, "r|def:a.F")
	if !ok || sym.Resolved || sym.Node != nil || sym.Type != "symbol" || len(sym.EdgeTypes) != 1 || sym.EdgeTypes[0] != "defines" {
		t.Fatalf("symbol peer = %+v, want pseudo-node {type:symbol}", sym)
	}
}

// TestNeighborsIncoming — incoming 1-hop follows to_key=centre and the user
// peer is a pseudo-node synthesized from from_type.
func TestNeighborsIncoming(t *testing.T) {
	q, sc := neighborFixtures()
	q.Direction = graph.Incoming
	res, err := graph.Neighbors(context.Background(), sc, q)
	if err != nil {
		t.Fatalf("Neighbors: %v", err)
	}
	if len(res.Neighbors) != 1 || res.Neighbors[0].Key != "user:gitlab.example#7" ||
		res.Neighbors[0].Resolved || res.Neighbors[0].Type != "user" {
		t.Fatalf("neighbors = %+v, want the incoming user pseudo-node", res.Neighbors)
	}
}

// TestNeighborsBoth — direction both unions outgoing and incoming peers,
// deduplicated.
func TestNeighborsBoth(t *testing.T) {
	q, sc := neighborFixtures()
	q.Direction = graph.DirectionBoth
	res, err := graph.Neighbors(context.Background(), sc, q)
	if err != nil {
		t.Fatalf("Neighbors: %v", err)
	}
	if len(res.Neighbors) != 3 {
		t.Fatalf("neighbors = %+v, want 3", res.Neighbors)
	}
	if _, ok := neighborByKey(res, "user:gitlab.example#7"); !ok {
		t.Fatalf("both must include the incoming user peer: %+v", res.Neighbors)
	}
}

// TestNeighborsRelFilter — rel_types restricts the 1-hop scan.
func TestNeighborsRelFilter(t *testing.T) {
	q, sc := neighborFixtures()
	q.Direction = graph.Outgoing
	q.RelTypes = []string{"imports"}
	res, err := graph.Neighbors(context.Background(), sc, q)
	if err != nil {
		t.Fatalf("Neighbors: %v", err)
	}
	if len(res.Neighbors) != 1 || res.Neighbors[0].Key != graph.RefPkg("r", "github.com/x/y") {
		t.Fatalf("neighbors = %+v, want only the imports peer", res.Neighbors)
	}
}

// TestNeighborsLimitTruncates — the neighbour list is shaped by the limit and
// flagged truncated.
func TestNeighborsLimitTruncates(t *testing.T) {
	q, sc := neighborFixtures()
	q.Direction = graph.Outgoing
	q.Limit = 1
	res, err := graph.Neighbors(context.Background(), sc, q)
	if err != nil {
		t.Fatalf("Neighbors: %v", err)
	}
	if !res.Truncated || len(res.Neighbors) != 1 {
		t.Fatalf("neighbors = %+v truncated=%v, want one flagged", res.Neighbors, res.Truncated)
	}
}

// TestNeighborsValidation — node_key is required and the direction vocabulary
// is validated before any scan.
func TestNeighborsValidation(t *testing.T) {
	if _, err := graph.Neighbors(context.Background(), &edgeScroller{}, graph.Query{}); err == nil {
		t.Fatal("missing node_key must be refused")
	}
	q := graph.Query{NodeKey: graph.RefFile("r", "a.go"), Direction: graph.Direction("sideways")}
	if _, err := graph.Neighbors(context.Background(), &edgeScroller{}, q); err == nil {
		t.Fatal("invalid direction must be refused")
	}
}
