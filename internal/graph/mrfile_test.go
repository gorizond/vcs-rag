package graph_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/qdrant/go-client/qdrant"

	"github.com/gorizond/vcs-rag/internal/graph"
	"github.com/gorizond/vcs-rag/internal/provider"
)

func TestMrFilePointDeterministicAndCapped(t *testing.T) {
	meta := graph.Meta{Repo: "org/repo", GroupOrg: "org", VCSType: "gitlab", Branch: "main", SchemaVer: "1", Generation: 7}
	file := provider.ChangeRequestFile{
		OldPath: "old.go", NewPath: "new.go", Status: "renamed", Patch: "small diff", MRID: 42,
	}
	p := graph.MrFilePoint(meta, 42, file, "alice", "opened")
	if p.GetId().GetUuid() != graph.MrFilePoint(meta, 42, file, "alice", "opened").GetId().GetUuid() {
		t.Fatal("MrFilePoint ID must be deterministic")
	}
	if path, _ := qdrant.ValueMapToMap(p.Payload)["file_path"].(string); path != "new.go" {
		t.Fatalf("file_path = %q, want the new (current) path", path)
	}
	if cut, _ := qdrant.ValueMapToMap(p.Payload)["patch_truncated"].(bool); cut {
		t.Fatal("patch_truncated is set on an uncut patch")
	}

	// a patch over the cap is cut on a rune boundary and flagged.
	long := strings.Repeat("a", graph.MaxPatchBytes-1) + strings.Repeat("é", 10)
	p = graph.MrFilePoint(meta, 42, provider.ChangeRequestFile{OldPath: "a.go", NewPath: "a.go", Status: "modified", Patch: long}, "", "closed")
	m := qdrant.ValueMapToMap(p.Payload)
	patch, _ := m["patch"].(string)
	if len(patch) > graph.MaxPatchBytes || !utf8.ValidString(patch) {
		t.Fatalf("capped patch: %d bytes, valid=%v", len(patch), utf8.ValidString(patch))
	}
	if cut, _ := m["patch_truncated"].(bool); !cut {
		t.Fatal("patch_truncated is not set on a cut patch")
	}

	// the provider's own truncation flag propagates even for a small patch.
	p = graph.MrFilePoint(meta, 42, provider.ChangeRequestFile{NewPath: "b.go", Status: "modified", Truncated: true}, "", "opened")
	if cut, _ := qdrant.ValueMapToMap(p.Payload)["patch_truncated"].(bool); !cut {
		t.Fatal("patch_truncated is not set for a provider-truncated file")
	}
}
