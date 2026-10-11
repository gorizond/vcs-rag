package graph_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/gorizond/vcs-rag/internal/graph"
)

// TestIdentityKeysDeterministicAndScoped pins the T003 Key* helpers: opaque
// NUL-separated uuid5 inputs, deterministic, and scoped by host/scope/item
// type/id so distinct entities never share an identity.
func TestIdentityKeysDeterministicAndScoped(t *testing.T) {
	for _, tc := range []struct {
		name  string
		key   string
		parts []string
	}{
		{"user", graph.KeyUser("gitlab.com", "42"), []string{"user", "gitlab.com", "42"}},
		{"note", graph.KeyNote("123"), []string{"note", "123"}},
		{"work item", graph.KeyWorkItem("issue", "7"), []string{"wi", "issue", "7"}},
		{"milestone", graph.KeyMilestone("8"), []string{"ms", "8"}},
		{"label", graph.KeyLabel("gorizond", "bug"), []string{"label", "gorizond", "bug"}},
		{"runner", graph.KeyRunner("gitlab.com", "9"), []string{"runner", "gitlab.com", "9"}},
	} {
		for _, p := range tc.parts {
			if !strings.Contains(tc.key, p) {
				t.Errorf("%s key %q misses part %q", tc.name, tc.key, p)
			}
		}
		if !strings.Contains(tc.key, "\x00") {
			t.Errorf("%s key %q is not NUL-separated", tc.name, tc.key)
		}
	}

	if graph.KeyUser("gitlab.com", "42") != graph.KeyUser("gitlab.com", "42") ||
		graph.KeyWorkItem("issue", "7") != graph.KeyWorkItem("issue", "7") {
		t.Fatal("identity keys must be deterministic")
	}

	seen := map[string]string{}
	for _, k := range []string{
		graph.KeyUser("gitlab.com", "42"),
		graph.KeyUser("github.com", "42"),
		graph.KeyLabel("gorizond", "bug"),
		graph.KeyLabel("other", "bug"),
		graph.KeyWorkItem("issue", "7"),
		graph.KeyWorkItem("merge_request", "7"),
		graph.KeyRunner("gitlab.com", "9"),
		graph.KeyRunner("gitlab.com", "10"),
	} {
		if prev, dup := seen[k]; dup {
			t.Errorf("distinct hosts/scopes/types/ids collide on %q (%q)", k, prev)
		}
		seen[k] = k
	}
}

// TestRefKeysScoped pins the payload (node_key/from_key/to_key) spellings: the
// same entity in another repo/host/scope must not collide.
func TestRefKeysScoped(t *testing.T) {
	if !strings.Contains(graph.RefWorkItem("org/repo", "issue", "7"), "org/repo") {
		t.Error("RefWorkItem must carry the repo")
	}
	if graph.RefWorkItem("org/repo", "issue", "7") == graph.RefWorkItem("org/other", "issue", "7") {
		t.Error("RefWorkItem must not collide across repos")
	}
	if !strings.Contains(graph.RefUser("gitlab.com", "42"), "gitlab.com") ||
		graph.RefUser("gitlab.com", "42") == graph.RefUser("github.com", "42") {
		t.Error("RefUser must be host-scoped")
	}
	if !strings.Contains(graph.RefLabel("gorizond", "bug"), "gorizond") ||
		graph.RefLabel("gorizond", "bug") == graph.RefLabel("other", "bug") {
		t.Error("RefLabel must be scope-scoped")
	}

	// F13 (schema v4): the commit / ci / diff-file canonical key formats.
	if got := graph.RefCommit("org/repo", "abc"); got != "org/repo|commit:abc" {
		t.Errorf("RefCommit = %q", got)
	}
	if got := graph.RefCi("org/repo", "job", "11", "7"); got != "org/repo|ci:job:11:7" {
		t.Errorf("RefCi (job) = %q", got)
	}
	if got := graph.RefCi("org/repo", "deployment", "11", ""); got != "org/repo|ci:deployment:11" {
		t.Errorf("RefCi (no job part) = %q", got)
	}
	if got := graph.RefMergeRequestDiffFile("org/repo", 7, "a/b.go"); got != "org/repo|mr:7|file:a/b.go" {
		t.Errorf("RefMergeRequestDiffFile = %q", got)
	}
}

// TestCompileModeValidation pins the T010 mode plumbing: the enum is accepted
// (empty → traversal), path_finding requires rel_types, and the selectivity
// guard holds for every mode.
func TestCompileModeValidation(t *testing.T) {
	if graph.ModeTraversal != "traversal" || graph.ModeAggregation != "aggregation" ||
		graph.ModePathFinding != "path_finding" || graph.ModeNeighbors != "neighbors" {
		t.Fatal("Mode constants changed: they are the DSL/MCP wire values")
	}

	sel := graph.Filter{Repo: "org/repo"}
	for _, tc := range []struct{ mode, want graph.Mode }{
		{"", graph.ModeTraversal},
		{graph.ModeTraversal, graph.ModeTraversal},
		{graph.ModeAggregation, graph.ModeAggregation},
		{graph.ModeNeighbors, graph.ModeNeighbors},
	} {
		c, err := graph.Compile(graph.Query{Mode: tc.mode, Start: sel, Hops: 1})
		if err != nil {
			t.Fatalf("mode %q: %v", tc.mode, err)
		}
		if c.Mode != tc.want {
			t.Fatalf("mode %q: compiled %q, want %q", tc.mode, c.Mode, tc.want)
		}
	}

	if _, err := graph.Compile(graph.Query{Mode: "scan", Start: sel}); err == nil {
		t.Fatal("unknown mode must be rejected")
	}

	_, err := graph.Compile(graph.Query{Mode: graph.ModePathFinding, Start: sel})
	if !errors.Is(err, graph.ErrRelTypesRequired) || !strings.Contains(err.Error(), "rel_types_required") {
		t.Fatalf("path_finding without rel_types: got %v, want ErrRelTypesRequired", err)
	}
	if _, err := graph.Compile(graph.Query{Mode: graph.ModePathFinding, Start: sel, RelTypes: []string{"imports"}, Hops: 2}); err != nil {
		t.Fatalf("path_finding with rel_types: %v", err)
	}

	// The selectivity guard is checked for every mode (and wins over the
	// rel_types check): no mode can smuggle a full-scan query through.
	for _, m := range []graph.Mode{graph.ModeTraversal, graph.ModeAggregation, graph.ModeNeighbors} {
		if _, err := graph.Compile(graph.Query{Mode: m, Start: graph.Filter{Branch: "main"}}); !errors.Is(err, graph.ErrNonSelective) {
			t.Fatalf("mode %q non-selective: got %v, want ErrNonSelective", m, err)
		}
	}
	if _, err := graph.Compile(graph.Query{
		Mode:     graph.ModePathFinding,
		Start:    graph.Filter{Branch: "main"},
		RelTypes: []string{"imports"},
	}); !errors.Is(err, graph.ErrNonSelective) {
		t.Fatalf("path_finding non-selective: got %v, want ErrNonSelective", err)
	}
}
