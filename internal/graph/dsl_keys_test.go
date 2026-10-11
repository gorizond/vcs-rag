package graph_test

import (
	"errors"
	"testing"

	"github.com/qdrant/go-client/qdrant"

	"github.com/gorizond/vcs-rag/internal/graph"
)

// T016 prerequisite: the canonical-key endpoints bound a query by themselves —
// a node_key-only neighbours query and a from/to path query pass Compile, a
// single endpoint or a node_key outside its mode does not.
func TestCompileCanonicalKeysSelective(t *testing.T) {
	from, to := graph.RefFile("r", "a.go"), graph.RefFile("r", "b.go")

	if _, err := graph.Compile(graph.Query{Mode: graph.ModeNeighbors, NodeKey: from}); err != nil {
		t.Fatalf("node_key-bounded neighbours query rejected: %v", err)
	}
	if _, err := graph.Compile(graph.Query{
		Mode: graph.ModePathFinding, FromKey: from, ToKey: to, RelTypes: []string{"imports"},
	}); err != nil {
		t.Fatalf("from/to-bounded path query rejected: %v", err)
	}
	// A raw node_key/from_key condition is a selective predicate in any mode.
	if _, err := graph.Compile(graph.Query{
		Start: graph.Filter{Conditions: []*qdrant.Condition{qdrant.NewMatch("node_key", from)}},
	}); err != nil {
		t.Fatalf("raw node_key condition rejected: %v", err)
	}

	// Genuinely non-selective queries stay refused: one path endpoint alone,
	// and a node_key smuggled into traversal (no key endpoint there).
	if _, err := graph.Compile(graph.Query{
		Mode: graph.ModePathFinding, FromKey: from, RelTypes: []string{"imports"},
	}); !errors.Is(err, graph.ErrNonSelective) {
		t.Fatalf("single-endpoint path query: got %v, want ErrNonSelective", err)
	}
	if _, err := graph.Compile(graph.Query{NodeKey: from}); !errors.Is(err, graph.ErrNonSelective) {
		t.Fatalf("node_key in traversal: got %v, want ErrNonSelective", err)
	}
}
