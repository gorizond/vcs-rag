package graph_test

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qdrant/go-client/qdrant"

	"github.com/gorizond/vcs-rag/internal/chunk"
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

// TestPackageName — the bare package name of an import path is its last
// segment, with a module major-version segment stripped.
func TestPackageName(t *testing.T) {
	for in, want := range map[string]string{
		"controllers":                        "controllers",
		"fmt":                                "fmt",
		"pkg/controllers":                    "controllers",
		"github.com/x/y/pkg/controllers":     "controllers",
		"example.com/lib/v2":                 "lib",
		"github.com/x/y/pkg/controllers/v10": "controllers",
		"":                                   "",
		"trailing/":                          "trailing",
	} {
		if got := graph.PackageName(in); got != want {
			t.Errorf("PackageName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestImportPointsToPackageName — import edges carry both package key forms:
// to_package (repo-namespaced full import path) and to_package_name (bare
// package name), the spelling definition nodes use. The payload field is
// additive: the deterministic edge ID is unchanged.
func TestImportPointsToPackageName(t *testing.T) {
	meta := graph.Meta{Repo: "org/repo", Branch: "main", SchemaVer: "1"}
	f := graph.FileGraph{Path: "pkg/a.go", Package: "a", Imports: []chunk.GraphImport{
		{Path: "github.com/org/repo/pkg/controllers"},
		{Path: "example.com/lib/v2"},
	}}
	pts := graph.ImportPoints(meta, f, nil)
	if len(pts) != 2 {
		t.Fatalf("points = %d, want 2", len(pts))
	}
	wantTo := []string{"github.com/org/repo/pkg/controllers", "example.com/lib/v2"}
	wantName := []string{"controllers", "lib"}
	for i, p := range pts {
		payload := qdrant.ValueMapToMap(p.GetPayload())
		if got := payload["to_package"]; got != graph.PkgKey("org/repo", wantTo[i]) {
			t.Errorf("point %d to_package = %v, want the full repo-scoped key", i, got)
		}
		if got := payload["to_package_name"]; got != wantName[i] {
			t.Errorf("point %d to_package_name = %v, want %q", i, got, wantName[i])
		}
	}
	// Same inputs → same IDs: the payload change does not break the sweep's
	// idempotent re-upsert.
	if pts[0].GetId().GetUuid() != graph.ImportPoints(meta, f, nil)[0].GetId().GetUuid() {
		t.Error("import edge IDs must stay deterministic")
	}
}
