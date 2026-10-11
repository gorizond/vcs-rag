package store

import (
	"testing"

	"github.com/qdrant/go-client/qdrant"
)

// TestGraphMatchAnyChunking pins the pure MatchAny helpers ScrollAll relies
// on: the empty-list short-circuit and the <=1000-per-request chunking.
func TestGraphMatchAnyChunking(t *testing.T) {
	// (a) an empty list can never match — no points, no scan.
	if !matchAnyShortCircuits(nil) || !matchAnyShortCircuits([]string{}) {
		t.Fatal("empty MatchAny must short-circuit")
	}
	if matchAnyShortCircuits([]string{"a"}) {
		t.Fatal("non-empty MatchAny must not short-circuit")
	}

	// (b) 2500 values split into 1000/1000/500, order preserved.
	in := make([]string, 2500)
	for i := range in {
		in[i] = string(rune('a' + i%26))
	}
	in[1000], in[2000] = "boundary1", "boundary2"
	chunks := chunkStrings(in, maxMatchAny)
	if len(chunks) != 3 {
		t.Fatalf("want 3 chunks, got %d", len(chunks))
	}
	var got []string
	for _, c := range chunks {
		if len(c) > maxMatchAny {
			t.Fatalf("chunk of %d exceeds maxMatchAny=%d", len(c), maxMatchAny)
		}
		got = append(got, c...)
	}
	if len(got) != len(in) {
		t.Fatalf("total preserved: want %d, got %d", len(in), len(got))
	}
	for i := range in {
		if got[i] != in[i] {
			t.Fatalf("order broken at %d: got %q, want %q", i, got[i], in[i])
		}
	}

	// (c) an input smaller than the chunk size yields exactly one chunk.
	small := chunkStrings([]string{"x", "y", "z"}, maxMatchAny)
	if len(small) != 1 || len(small[0]) != 3 {
		t.Fatalf("small input: want one 3-element chunk, got %v", small)
	}
}

func containsField(fields []string, want string) bool {
	for _, f := range fields {
		if f == want {
			return true
		}
	}
	return false
}

// TestGraphIndexFieldsOrbitParity pins the T004 index declaration: the
// canonical key/type fields, the new node-family fields and their datetime
// classification, with body/title deliberately unindexed.
func TestGraphIndexFieldsOrbitParity(t *testing.T) {
	for _, f := range []string{
		"node_key", "from_key", "to_key", "from_type", "to_type",
		"vcs_user_id", "login", "name", "parent_key", "author_key",
		"item_id", "item_type", "ms_id", "label_key", "scope",
		"runner_id", "runner_type", "tags",
		"created_at", "updated_at", "due_date", "last_seen_at",
	} {
		if !containsField(graphIndexFields, f) {
			t.Errorf("graphIndexFields misses %q", f)
		}
	}
	for _, f := range []string{"body", "title"} {
		if containsField(graphIndexFields, f) || graphOnDiskIndexFields[f] {
			t.Errorf("%q must never be indexed (bodies stay unindexed)", f)
		}
	}
	for _, f := range []string{"created_at", "updated_at", "due_date", "last_seen_at"} {
		if !graphDatetimeIndexFields[f] {
			t.Errorf("%q must be classified as datetime", f)
		}
	}
	for _, f := range []string{"node_key", "from_key", "to_key", "parent_key"} {
		if !graphOnDiskIndexFields[f] {
			t.Errorf("%q must be indexed on disk", f)
		}
	}
}

// TestFacetValueKey pins the facet value rendering: keyword facets arrive as
// strings, integer/datetime facets as integers, bool facets as bools.
func TestFacetValueKey(t *testing.T) {
	for _, tc := range []struct {
		v    *qdrant.FacetValue
		want string
	}{
		{qdrant.NewFacetValue("imports"), "imports"},
		{&qdrant.FacetValue{Variant: &qdrant.FacetValue_IntegerValue{IntegerValue: 42}}, "42"},
		{&qdrant.FacetValue{Variant: &qdrant.FacetValue_IntegerValue{IntegerValue: -1}}, "-1"},
		{&qdrant.FacetValue{Variant: &qdrant.FacetValue_BoolValue{BoolValue: true}}, "true"},
		{&qdrant.FacetValue{Variant: &qdrant.FacetValue_BoolValue{BoolValue: false}}, "false"},
		{&qdrant.FacetValue{}, ""},
	} {
		if got := facetValueKey(tc.v); got != tc.want {
			t.Errorf("facetValueKey(%v) = %q, want %q", tc.v, got, tc.want)
		}
	}
}
