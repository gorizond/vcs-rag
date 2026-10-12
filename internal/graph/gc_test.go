package graph_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/qdrant/go-client/qdrant"

	"github.com/gorizond/vcs-rag/internal/graph"
)

// sweepFake is a hermetic store seam for graph.Sweep (the unexported
// graph.sweepStore method set): it serves a static stale-point count per kind
// and records what the sweep deletes, so the gating contract of T047 is
// verified without a Qdrant server.
type sweepFake struct {
	stale   map[graph.Kind]int
	deleted map[graph.Kind]int
}

// filterKind reads back the kind a sweep filter selects (staleKindFilter and
// cascadeByField both carry it as the first `kind` condition).
func filterKind(flt *qdrant.Filter) graph.Kind {
	for _, c := range flt.GetMust() {
		fc := c.GetField()
		if fc == nil || fc.GetKey() != "kind" {
			continue
		}
		return graph.Kind(fc.GetMatch().GetKeyword())
	}
	return ""
}

func (f *sweepFake) CountByFilter(_ context.Context, flt *qdrant.Filter) (uint64, error) {
	return uint64(f.stale[filterKind(flt)]), nil
}

func (f *sweepFake) DeleteByFilter(_ context.Context, flt *qdrant.Filter) error {
	k := filterKind(flt)
	f.deleted[k] += f.stale[k]
	return nil
}

func (f *sweepFake) ScrollAll(_ context.Context, _ *qdrant.Filter, _ uint32) ([]*qdrant.RetrievedPoint, error) {
	return nil, nil
}

// TestSweepGatesBestEffortKinds — T047: the flattened ci family and the legacy
// vuln points are swept only when their listing-complete flag is set. A
// withheld flag keeps their stale points, a set one generation-sweeps exactly
// them, and the core kinds stay unconditional.
func TestSweepGatesBestEffortKinds(t *testing.T) {
	stale := map[graph.Kind]int{graph.KindCi: 2, graph.KindVuln: 1, graph.KindNode: 3}
	ctx := context.Background()

	// Withheld: neither best-effort family may be deleted; core kinds sweep.
	held := &sweepFake{stale: stale, deleted: map[graph.Kind]int{}}
	if _, err := graph.Sweep(ctx, held, "r", "main", 10, false, graph.SweepTargets{}); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if held.deleted[graph.KindCi] != 0 || held.deleted[graph.KindVuln] != 0 {
		t.Fatalf("withheld flags must keep the stale points: deleted=%v", held.deleted)
	}
	if held.deleted[graph.KindNode] != 3 {
		t.Fatalf("core kinds stay unconditional: deleted=%v", held.deleted)
	}

	// Set: a completed listing releases exactly its kind.
	done := &sweepFake{stale: stale, deleted: map[graph.Kind]int{}}
	if _, err := graph.Sweep(ctx, done, "r", "main", 10, false, graph.SweepTargets{CI: true, Vuln: true}); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if done.deleted[graph.KindCi] != 2 || done.deleted[graph.KindVuln] != 1 {
		t.Fatalf("completed listings must sweep their kinds: deleted=%v", done.deleted)
	}

	// Gating is per kind: a completed CI listing leaves the legacy vuln family
	// of a failed security listing untouched.
	ciOnly := &sweepFake{stale: stale, deleted: map[graph.Kind]int{}}
	if _, err := graph.Sweep(ctx, ciOnly, "r", "main", 10, false, graph.SweepTargets{CI: true}); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if ciOnly.deleted[graph.KindCi] != 2 || ciOnly.deleted[graph.KindVuln] != 0 {
		t.Fatalf("gates are per-kind: deleted=%v", ciOnly.deleted)
	}
}

// TestSweepOwnershipInstanceWide — the pass-level ownership sweep is
// repo-agnostic (kind + generation alone, no repo/branch scope): it deletes
// the stale group and project points of the whole instance and touches no
// other kind. The per-repo Sweep leaves both kinds alone.
func TestSweepOwnershipInstanceWide(t *testing.T) {
	ctx := context.Background()
	stale := map[graph.Kind]int{graph.KindGroup: 2, graph.KindProject: 5, graph.KindNode: 3}

	gs := &sweepFake{stale: stale, deleted: map[graph.Kind]int{}}
	if _, err := graph.Sweep(ctx, gs, "r", "main", 10, false, graph.SweepTargets{}); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if gs.deleted[graph.KindGroup] != 0 || gs.deleted[graph.KindProject] != 0 {
		t.Fatalf("per-repo Sweep must leave the ownership kinds alone: deleted=%v", gs.deleted)
	}

	gs = &sweepFake{stale: stale, deleted: map[graph.Kind]int{}}
	deleted, err := graph.SweepOwnership(ctx, gs, 10, false)
	if err != nil {
		t.Fatalf("SweepOwnership: %v", err)
	}
	if deleted != 7 || gs.deleted[graph.KindGroup] != 2 || gs.deleted[graph.KindProject] != 5 {
		t.Fatalf("SweepOwnership deleted=%d %v, want both ownership kinds (2+5)", deleted, gs.deleted)
	}
	if gs.deleted[graph.KindNode] != 0 {
		t.Fatalf("SweepOwnership touched a foreign kind: %v", gs.deleted)
	}
}

// TestSweepOwnershipZeroTrace — the instance-level sweep is traceable even
// when nothing matches: each kind logs its scanned count (a zero included) and
// the run logs an entry and a completion line with the total deleted.
func TestSweepOwnershipZeroTrace(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	gs := &sweepFake{stale: map[graph.Kind]int{}, deleted: map[graph.Kind]int{}}
	n, err := graph.SweepOwnership(context.Background(), gs, 10, false)
	if err != nil || n != 0 {
		t.Fatalf("SweepOwnership: n=%d err=%v, want 0,nil", n, err)
	}
	out := buf.String()
	for _, want := range []string{
		"gc scan", "kind=group", "kind=project",
		"generation_below=10", "points=0",
		"gc sweep start", "scope=instance", "gc sweep done",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("zero-count sweep log misses %q:\n%s", want, out)
		}
	}
}
