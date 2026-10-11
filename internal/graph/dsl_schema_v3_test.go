package graph_test

import (
	"errors"
	"testing"

	"github.com/qdrant/go-client/qdrant"

	"github.com/gorizond/vcs-rag/internal/graph"
)

// hasKind reports whether the must chain pins kind to value, either as a
// single keyword or inside a match-any list.
func hasKind(f *qdrant.Filter, value string) bool {
	for _, c := range f.GetMust() {
		fc := c.GetField()
		if fc == nil || fc.GetKey() != "kind" {
			continue
		}
		if fc.GetMatch().GetKeyword() == value {
			return true
		}
		for _, v := range fc.GetMatch().GetKeywords().GetStrings() {
			if v == value {
				return true
			}
		}
	}
	return false
}

// T006/T007: the canonical v3 entity spellings resolve to both kind spellings
// of their legacy family (FR-019), every schema-v3 family compiles to its own
// kind, and every schema-v3 edge type passes validation.
func TestCompileSchemaV3EntitiesAndEdges(t *testing.T) {
	for _, tc := range []struct {
		entity graph.Entity
		kinds  []string
	}{
		// T006 aliases: canonical resolves to both spellings.
		{graph.EntityMergeRequestDiffFile, []string{"merge_request_diff_file", "mr_file"}},
		{graph.EntityVulnerability, []string{"vulnerability", "vuln"}},
		// T007 new start entities: kind = entity name.
		{graph.EntityGroup, []string{"group"}},
		{graph.EntityProject, []string{"project"}},
		{graph.EntityBranch, []string{"branch"}},
		{graph.EntityDirectory, []string{"directory"}},
		{graph.EntityImportedSymbol, []string{"imported_symbol"}},
		{graph.EntityMergeRequest, []string{"merge_request"}},
		{graph.EntityMergeRequestDiff, []string{"merge_request_diff"}},
		{graph.EntitySecurityScan, []string{"security_scan"}},
		{graph.EntityFinding, []string{"finding"}},
		{graph.EntityVulnerabilityOccurrence, []string{"vulnerability_occurrence"}},
		{graph.EntityVulnerabilityIdentifier, []string{"vulnerability_identifier"}},
		{graph.EntityVulnerabilityScanner, []string{"vulnerability_scanner"}},
	} {
		c, err := graph.Compile(graph.Query{StartEntity: tc.entity, Start: graph.Filter{Repo: "r"}})
		if err != nil {
			t.Fatalf("entity %q: %v", tc.entity, err)
		}
		for _, k := range tc.kinds {
			if !hasKind(c.Start, k) {
				t.Errorf("entity %q: start filter misses kind %q: %v", tc.entity, k, c.Start)
			}
		}
	}
	for _, et := range []graph.EdgeType{
		graph.EdgeContains, graph.EdgeOnBranch, graph.EdgeHasDiff, graph.EdgeHasLatestDiff,
		graph.EdgeHasFile, graph.EdgeMerged, graph.EdgeReviewer, graph.EdgeInProject,
		graph.EdgeInPipeline, graph.EdgeHasFinding, graph.EdgeOccurrenceOf, graph.EdgeDetectedBy,
		graph.EdgeHasIdentifier, graph.EdgeFixes, graph.EdgeScans, graph.EdgeResolvedBy,
		graph.EdgeConfirmedBy, graph.EdgeDismissedBy, graph.EdgeMergedAtCommit,
	} {
		if _, err := graph.Compile(graph.Query{Start: graph.Filter{Repo: "r"}, EdgeType: et}); err != nil {
			t.Errorf("edge type %q rejected: %v", et, err)
		}
	}
	if _, err := graph.Compile(graph.Query{Start: graph.Filter{Repo: "r"}, EdgeType: "calls"}); err == nil {
		t.Error("unknown edge type accepted")
	}
}

// T012: the schema-v3 identity fields are selective predicates; a query
// without any selective predicate still keeps the non_selective_query refusal.
func TestCompileSchemaV3SelectivePredicates(t *testing.T) {
	for _, field := range []string{"group_id", "project_id", "scan_id", "scanner_key", "primary_identifier_key", "mr_key", "dir_key"} {
		if _, err := graph.Compile(graph.Query{Start: graph.Filter{
			Conditions: []*qdrant.Condition{qdrant.NewMatch(field, "x")},
		}}); err != nil {
			t.Errorf("selective predicate %q rejected: %v", field, err)
		}
	}
	if _, err := graph.Compile(graph.Query{Start: graph.Filter{Branch: "main"}, Hops: 1}); !errors.Is(err, graph.ErrNonSelective) {
		t.Fatalf("no selective predicate: got %v, want ErrNonSelective", err)
	}
}
