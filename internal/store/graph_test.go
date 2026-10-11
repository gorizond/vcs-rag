package store

import "testing"

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
