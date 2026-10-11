package graph

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/qdrant/go-client/qdrant"

	"github.com/gorizond/vcs-rag/internal/store"
)

// SweepTargets reports which orbit-family listings of one completed pass were
// complete. Only a kind whose listing succeeded may be swept: a partial or
// failed listing would otherwise delete live points the pass simply did not
// re-see (T031). The core kinds (node/edge/mr_file/ci/vuln) are not gated here
// — their writers fail the pass on a listing error, so a pass that reaches the
// sweep always saw them fully.
type SweepTargets struct {
	Users      bool
	WorkItems  bool
	Milestones bool
	Labels     bool
}

// Sweep deletes stale-generation points of one repository+branch, one kind at
// a time (mark-and-sweep, T011), and returns how many points were deleted (or,
// in dry-run, would be). A point is stale when its payload generation is lower
// than the current one; points of the current generation are never touched.
//
// Orbit kinds follow their own rules (T027; data-model §9): users/labels/
// milestones/work_items are generation-swept exactly like the core kinds, but
// only when their pass listing completed (SweepTargets). Users are host-scoped
// by construction (node_key RefUser): within one repo+branch every user point
// comes from the one provider instance the process serves. Notes are NEVER
// generation-swept (R9): a live note the pass did not re-list must survive, so
// they are cascade-deleted only when their parent is being removed — a stale
// work item (gated by its complete listing) or a change request whose file
// points are stale. Their commented edges ride the normal edge sweep and are
// re-created with the note.
//
// No TTL is applied to notes (the optional part of R9): a note whose parent
// survives but whose own deletion the provider never signals lingers; add a
// created_at TTL here if that drift matters.
//
// dryRun comes from WORKER_GC_DRY_RUN (config.Worker.GCDryRun): when set, only
// "gc would-delete" log lines are written and nothing is deleted.
//
// The caller MUST call Sweep only for a repo+branch whose graph pass has
// completed successfully and is not in flight: a failed or running pass may
// still write points of the previous generation, which Sweep would delete
// mid-pass (the graph lane owns that guarantee).
func Sweep(ctx context.Context, gs *store.GraphStore, repo, branch string, generation int, dryRun bool, t SweepTargets) (deleted int, err error) {
	// Cascade the notes of the parents this sweep is about to remove: collect
	// their canonical refs BEFORE anything is deleted, then delete the notes.
	var parents []string
	if t.WorkItems {
		refs, err := staleParentRefs(ctx, gs, staleKindFilter(KindWorkItem, repo, branch, generation), "node_key")
		if err != nil {
			return deleted, err
		}
		parents = append(parents, refs...)
	}
	// A change request whose mr_file points are stale is gone: its notes
	// cascade through the MR parent ref (GitLab MRs are not work-item nodes;
	// on GitHub PR notes hang off the PR's work-item node, so these refs match
	// nothing — the work-item cascade above already owns them).
	mrIDs, err := staleParentRefs(ctx, gs, staleKindFilter(KindMrFile, repo, branch, generation), "mr_id")
	if err != nil {
		return deleted, err
	}
	for _, id := range mrIDs {
		parents = append(parents, RefWorkItem(repo, "merge_request", id))
	}
	n, err := cascadeNotes(ctx, gs, parents, dryRun)
	if err != nil {
		return deleted, err
	}
	deleted += n

	for _, kind := range []Kind{KindNode, KindEdge, KindMrFile, KindCi, KindVuln} {
		n, err := sweepKind(ctx, gs, kind, repo, branch, generation, dryRun)
		if err != nil {
			return deleted, err
		}
		deleted += n
	}
	for _, gated := range []struct {
		kind Kind
		ok   bool
	}{
		{KindWorkItem, t.WorkItems},
		{KindUser, t.Users},
		{KindMilestone, t.Milestones},
		{KindLabel, t.Labels},
	} {
		if !gated.ok {
			continue
		}
		n, err := sweepKind(ctx, gs, gated.kind, repo, branch, generation, dryRun)
		if err != nil {
			return deleted, err
		}
		deleted += n
	}
	return deleted, nil
}

// sweepKind counts and deletes (dry-run: only reports) the stale points of one
// kind.
func sweepKind(ctx context.Context, gs *store.GraphStore, kind Kind, repo, branch string, generation int, dryRun bool) (int, error) {
	flt := staleKindFilter(kind, repo, branch, generation)
	n, err := gs.CountByFilter(ctx, flt)
	if err != nil {
		return 0, fmt.Errorf("graph gc: count %s: %w", kind, err)
	}
	if n == 0 {
		return 0, nil
	}
	if dryRun {
		slog.Info("gc would-delete",
			"kind", kind, "repo", repo, "branch", branch,
			"generation_below", generation, "points", n)
		return int(n), nil
	}
	if err := gs.DeleteByFilter(ctx, flt); err != nil {
		return 0, fmt.Errorf("graph gc: delete %s: %w", kind, err)
	}
	slog.Info("gc deleted",
		"kind", kind, "repo", repo, "branch", branch,
		"generation_below", generation, "points", n)
	return int(n), nil
}

// staleParentRefs scrolls the points a sweep is about to remove and collects
// the distinct values of one payload key — the canonical parent refs of the
// notes to cascade.
func staleParentRefs(ctx context.Context, gs *store.GraphStore, flt *qdrant.Filter, payloadKey string) ([]string, error) {
	pts, err := gs.ScrollAll(ctx, flt, 256)
	if err != nil {
		return nil, fmt.Errorf("graph gc: scroll stale %s: %w", payloadKey, err)
	}
	seen := make(map[string]bool, len(pts))
	out := make([]string, 0, len(pts))
	for _, p := range pts {
		v := p.GetPayload()[payloadKey].GetStringValue()
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out, nil
}

// noteParentChunk bounds the parent_key MatchAny of one cascade request: the
// store chunks MatchAny only for scrolls, and its per-request ceiling is 1000
// values (store.maxMatchAny).
const noteParentChunk = 1000

// cascadeNotes deletes every note whose parent_key is one of parents — the
// cascade half of the notes GC (T027/R9). It respects dryRun like every other
// sweep.
func cascadeNotes(ctx context.Context, gs *store.GraphStore, parents []string, dryRun bool) (int, error) {
	if len(parents) == 0 {
		return 0, nil
	}
	deleted := 0
	for start := 0; start < len(parents); start += noteParentChunk {
		end := min(start+noteParentChunk, len(parents))
		flt := &qdrant.Filter{Must: []*qdrant.Condition{
			qdrant.NewMatch("kind", string(KindNote)),
			qdrant.NewMatchKeywords("parent_key", parents[start:end]...),
		}}
		n, err := gs.CountByFilter(ctx, flt)
		if err != nil {
			return deleted, fmt.Errorf("graph gc: count note: %w", err)
		}
		if n == 0 {
			continue
		}
		if dryRun {
			slog.Info("gc would-delete",
				"kind", KindNote, "parent_key_in", end-start, "points", n)
			deleted += int(n)
			continue
		}
		if err := gs.DeleteByFilter(ctx, flt); err != nil {
			return deleted, fmt.Errorf("graph gc: delete note: %w", err)
		}
		slog.Info("gc deleted",
			"kind", KindNote, "parent_key_in", end-start, "points", n)
		deleted += int(n)
	}
	return deleted, nil
}

// staleKindFilter selects one kind's points of repo+branch with
// generation < current (payload fields written by the graph lane).
func staleKindFilter(kind Kind, repo, branch string, generation int) *qdrant.Filter {
	return &qdrant.Filter{Must: []*qdrant.Condition{
		qdrant.NewMatch("kind", string(kind)),
		qdrant.NewMatch("repo", repo),
		qdrant.NewMatch("branch", branch),
		qdrant.NewRange("generation", &qdrant.Range{Lt: qdrant.PtrOf(float64(generation))}),
	}}
}
