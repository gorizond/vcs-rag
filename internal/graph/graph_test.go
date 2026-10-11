package graph_test

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gorizond/vcs-rag/internal/graph"
)

// TestNoEmbeddingImports enforces invariant R3: internal/graph must not depend
// on the embedding package or any ML client.
func TestNoEmbeddingImports(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob graph sources: %v (files=%d)", err, len(files))
	}
	fset := token.NewFileSet()
	for _, f := range files {
		ast, err := parser.ParseFile(fset, f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		for _, imp := range ast.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if strings.Contains(path, "embed") || strings.Contains(path, "ml") {
				t.Errorf("%s imports forbidden package %q (R3: no embedding/ML deps)", f, path)
			}
		}
	}
}

func TestIDsDeterministicAndRepoScoped(t *testing.T) {
	ns := graph.Namespace("1")
	if ns != graph.Namespace("1") {
		t.Fatal("Namespace must be deterministic")
	}
	if graph.Namespace("2") == ns {
		t.Fatal("Namespace must change with schema version")
	}
	if ns[6]>>4 != 5 {
		t.Fatalf("Namespace is not uuid v5: version nibble %d", ns[6]>>4)
	}

	a := graph.NodeID(ns, "org/repo", "main", graph.KindNode, graph.KeyFile("a.go"))
	if a != graph.NodeID(ns, "org/repo", "main", graph.KindNode, graph.KeyFile("a.go")) {
		t.Fatal("NodeID must be deterministic")
	}
	if a == graph.NodeID(ns, "org/repo", "dev", graph.KindNode, graph.KeyFile("a.go")) {
		t.Fatal("NodeID must vary by branch")
	}

	e := graph.EdgeID(ns, "org/repo", "main", graph.EdgeImports, graph.KeyFile("a.go"), graph.PkgKey("org/repo", "pkg"))
	if e != graph.EdgeID(ns, "org/repo", "main", graph.EdgeImports, graph.KeyFile("a.go"), graph.PkgKey("org/repo", "pkg")) {
		t.Fatal("EdgeID must be deterministic")
	}

	pk := graph.PkgKey("org/repo", "utils")
	if !strings.Contains(pk, "org/repo") || !strings.Contains(pk, "utils") {
		t.Fatalf("PkgKey must include repo and package: %q", pk)
	}
	if pk == graph.PkgKey("org/other", "utils") {
		t.Fatal("PkgKey must not collide across repos")
	}
}
