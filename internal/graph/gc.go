package graph

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/qdrant/go-client/qdrant"
)

// SweepTargets reports which best-effort family listings of one completed pass
// were complete. Only a kind whose listing succeeded may be swept: a partial or
// failed listing would otherwise delete live points the pass simply did not
// re-see (T031). The core kinds (node/edge/mr_file plus the code-graph-derived
// file/directory/imported_symbol) are not gated here — their writers fail the
// pass on a listing error, so a pass that reaches the sweep always saw them
// fully. Every other family rides its own flag.
type SweepTargets struct {
	Users      bool
	WorkItems  bool
	Milestones bool
	Labels     bool
	Runners    bool
	// Security gates the schema-v3 security sub-family (vulnerability, finding,
	// occurrence, identifier, security_scan, scanner): one runtime-gated
	// listing covers them all (findings carry their scanner/identifier/scan
	// context), so a latched-off or failed listing must keep their stale points
	// instead of deleting them blindly.
	Security bool
	// Review gates the change-request sub-family (merge_request,
	// merge_request_diff, merge_request_diff_file, and the commit nodes the
	// same listing emits): its ingest is one best-effort request listing plus
	// per-request diff listings, so a failed or withheld listing must keep
	// their stale points.
	Review bool
	// Ownership gates the group/project chain: its ingest is the best-effort
	// group listing plus the per-group project listings.
	Ownership bool
	// Structure gates the branch nodes: their ingest is the best-effort branch
	// listing. The file/directory/imported_symbol points of the same family
	// stay core — the code-graph write step fails the pass on error.
	Structure bool
	// CI gates the flattened CI family (ci): its ingest is one best-effort
	// pipeline listing plus the per-pipeline job, deployment and environment
	// listings, so a failed listing must keep their stale points.
	CI bool
	// Vuln gates the legacy vuln points: their ingest is the runtime-gated
	// findings listing (the same provider domain t.Security gates the schema-v3
	// vulnerability family from), so a latched-off or failed listing must keep
	// their stale points instead of deleting them blindly.
	Vuln bool
}

// sweepStore is the store seam of the sweep: the production
// *store.GraphStore satisfies this method set, and gc_test.go fakes it to prove
// the gating contract without a Qdrant server.
type sweepStore interface {
	CountByFilter(ctx context.Context, f *qdrant.Filter) (uint64, error)
	DeleteByFilter(ctx context.Context, f *qdrant.Filter) error
	ScrollAll(ctx context.Context, filter *qdrant.Filter, limit uint32) ([]*qdrant.RetrievedPoint, error)
}

// Sweep deletes stale-generation points of one repository+branch, one kind at
// a time (mark-and-sweep, T011), and returns how many points were deleted (or,
// in dry-run, would be). A point is stale when its payload generation is lower
// than the current one; points of the current generation are never touched.
//
// Orbit kinds follow their own rules (T027; data-model §9): users/labels/
// milestones/work_items/runners are generation-swept exactly like the core
// kinds, but only when their pass listing completed (SweepTargets). Users are
// host-scoped by construction (node_key RefUser): within one repo+branch every
// user point comes from the one provider instance the process serves. The same
// holds for runners (node_key RefRunner). The schema-v3 review/ownership/
// structure families and the flattened ci / legacy vuln families are gated the
// same way (t.Review/t.Ownership/t.Structure/t.CI/t.Vuln): their providers list
// best-effort, so a withheld listing keeps their stale points instead of
// deleting them blindly. The ownership kinds are additionally swept
// repo-agnostically: their points are instance-level (empty payload scope), so
// the per-repo filter never matches them — see the ownership block below the
// gated loop. Notes are NEVER
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
// The schema-v3 sub-objects cascade with their parents by the same
// collect-then-delete pattern (data-model §invariants: difffile⊂diff⊂mr,
// occurrence⊂vulnerability): a child carries the parent's canonical ref in its
// linkage field (mr_key, vulnerability_id) and is removed before the parent
// points disappear. The child families are generation-swept on their own; the
// cascade additionally covers a child whose own listing was withheld. A gated
// parent cascades nothing while its own sweep is withheld — a partial listing
// must never delete live children.
//
// dryRun comes from WORKER_GC_DRY_RUN (config.Worker.GCDryRun): when set, only
// "gc would-delete" log lines are written and nothing is deleted.
//
// The caller MUST call Sweep only for a repo+branch whose graph pass has
// completed successfully and is not in flight: a failed or running pass may
// still write points of the previous generation, which Sweep would delete
// mid-pass (the graph lane owns that guarantee).
func Sweep(ctx context.Context, gs sweepStore, repo, branch string, generation int, dryRun bool, t SweepTargets) (deleted int, err error) {
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

	// Schema-v3 sub-objects cascade with their stale parents (T008, data-model
	// §invariants). Refs are collected before the kind loops delete the parents.
	n, err = cascadeSubObjects(ctx, gs, repo, branch, generation, dryRun, t)
	if err != nil {
		return deleted, err
	}
	deleted += n

	// Core sweep (T008): the kinds whose writer fails the pass on a listing
	// error, so a pass that reaches the sweep always saw them fully —
	// node/edge/mr_file plus the code-graph-derived structure nodes
	// (file/directory) and imported_symbol. The best-effort families (ci,
	// vuln, review, ownership, structure, security, orbit) ride their
	// SweepTargets flag below. mr_file stays: legacy points coexist with the
	// canonical merge_request_diff_file kind until the reindex.
	for _, kind := range []Kind{
		KindNode, KindEdge, KindMrFile,
		KindDirectory, KindFile, KindImportedSymbol,
	} {
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
		{KindRunner, t.Runners},
		// Flattened CI family: the best-effort pipeline/job/deployment/
		// environment listings gate the ci points.
		{KindCi, t.CI},
		// Legacy vuln points: the runtime-gated findings listing gates them
		// (the schema-v3 vulnerability family has its own t.Security below).
		{KindVuln, t.Vuln},
		// Review sub-family: the best-effort request/diff listings gate all
		// three kinds.
		{KindMergeRequest, t.Review},
		{KindMergeRequestDiff, t.Review},
		{KindMergeRequestDiffFile, t.Review},
		// F13 (schema v4): commit nodes are emitted from the same MR listing.
		{KindCommit, t.Review},
		// Ownership chain: the best-effort group/project listings gate both
		// kinds. This per-repo pass clears the pre-fix duplicate points that
		// carry the scanning repo; the scope-free points are swept below.
		{KindGroup, t.Ownership},
		{KindProject, t.Ownership},
		// Structure: the best-effort branch listing gates the branch nodes
		// (file/directory/imported_symbol stay core above).
		{KindBranch, t.Structure},
		// Security sub-family (T008): all six kinds ride t.Security — they come
		// from the one runtime-gated findings listing.
		{KindVulnerability, t.Security},
		{KindFinding, t.Security},
		{KindVulnerabilityOccurrence, t.Security},
		{KindVulnerabilityIdentifier, t.Security},
		{KindSecurityScan, t.Security},
		{KindVulnerabilityScanner, t.Security},
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
	// Ownership is instance-level: the group/project points are built with an
	// empty scope, so the per-repo loop above never matches them. When the
	// group listings completed (t.Ownership), sweep them by kind + generation
	// alone — a group/project that disappeared from the listing goes away
	// regardless of which repo pass last wrote it.
	if t.Ownership {
		for _, kind := range []Kind{KindGroup, KindProject} {
			n, err := sweepInstanceKind(ctx, gs, kind, generation, dryRun)
			if err != nil {
				return deleted, err
			}
			deleted += n
		}
	}
	return deleted, nil
}

// sweepKind counts and deletes (dry-run: only reports) the stale points of one
// kind of one repo+branch.
func sweepKind(ctx context.Context, gs sweepStore, kind Kind, repo, branch string, generation int, dryRun bool) (int, error) {
	return sweepStale(ctx, gs, kind, staleKindFilter(kind, repo, branch, generation), generation, dryRun,
		"repo", repo, "branch", branch)
}

// sweepInstanceKind counts and deletes (dry-run: only reports) the stale points
// of one kind across the whole instance (no repo/branch scope): the ownership
// kinds are written with an empty scope, so the per-repo filter never matches
// them.
func sweepInstanceKind(ctx context.Context, gs sweepStore, kind Kind, generation int, dryRun bool) (int, error) {
	return sweepStale(ctx, gs, kind, staleInstanceKindFilter(kind, generation), generation, dryRun)
}

// sweepStale is the shared count/delete/report body of sweepKind and
// sweepInstanceKind; logAttrs carry the optional scope of the selection.
func sweepStale(ctx context.Context, gs sweepStore, kind Kind, flt *qdrant.Filter, generation int, dryRun bool, logAttrs ...any) (int, error) {
	n, err := gs.CountByFilter(ctx, flt)
	if err != nil {
		return 0, fmt.Errorf("graph gc: count %s: %w", kind, err)
	}
	if n == 0 {
		return 0, nil
	}
	attrs := append([]any{"kind", kind}, logAttrs...)
	attrs = append(attrs, "generation_below", generation, "points", n)
	if dryRun {
		slog.Info("gc would-delete", attrs...)
		return int(n), nil
	}
	if err := gs.DeleteByFilter(ctx, flt); err != nil {
		return 0, fmt.Errorf("graph gc: delete %s: %w", kind, err)
	}
	slog.Info("gc deleted", attrs...)
	return int(n), nil
}

// staleParentRefs scrolls the points a sweep is about to remove and collects
// the distinct values of one payload key — the canonical parent refs of the
// notes to cascade.
func staleParentRefs(ctx context.Context, gs sweepStore, flt *qdrant.Filter, payloadKey string) ([]string, error) {
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

// noteParentChunk bounds the refs MatchAny of one cascade request: the store
// chunks MatchAny only for scrolls, and its per-request ceiling is 1000 values
// (store.maxMatchAny).
const noteParentChunk = 1000

// cascadeNotes deletes every note whose parent_key is one of parents — the
// cascade half of the notes GC (T027/R9). It respects dryRun like every other
// sweep.
func cascadeNotes(ctx context.Context, gs sweepStore, parents []string, dryRun bool) (int, error) {
	return cascadeByField(ctx, gs, KindNote, "parent_key", parents, dryRun)
}

// subObjectCascade is one parent→child sweep rule of the schema-v3 families
// (T008, data-model §invariants): when a sweep removes stale points of parent,
// the child points whose childField carries the parent's canonical ref (its
// node_key) are deleted with it. gate names the parent family's SweepTargets
// flag: a withheld listing skips the rule, so it cascades nothing.
type subObjectCascade struct {
	parent     Kind
	child      Kind
	childField string
	gate       func(SweepTargets) bool
}

// subObjectCascades are the declared sub-object rules. The diff_file family is
// reached transitively: its mr_key carries the merge_request ref its diff
// carries too, so removing a stale MR removes the whole subtree. The
// merge_request cascade rides t.Review (the request listing is best-effort);
// the vulnerability cascade rides t.Security.
var subObjectCascades = []subObjectCascade{
	{parent: KindMergeRequest, child: KindMergeRequestDiff, childField: "mr_key", gate: reviewTarget},
	{parent: KindMergeRequest, child: KindMergeRequestDiffFile, childField: "mr_key", gate: reviewTarget},
	{parent: KindVulnerability, child: KindVulnerabilityOccurrence, childField: "vulnerability_id", gate: securityTarget},
}

// reviewTarget / securityTarget adapt the SweepTargets flags to the cascade
// gate signature.
func reviewTarget(t SweepTargets) bool   { return t.Review }
func securityTarget(t SweepTargets) bool { return t.Security }

// cascadeSubObjects applies the sub-object cascade rules of one sweep: the
// stale parents' canonical refs are collected first, then their children are
// deleted by the linkage field. Rules whose parent family is gated by a
// withheld listing are skipped (t): a partial listing must not delete live
// children.
func cascadeSubObjects(ctx context.Context, gs sweepStore, repo, branch string, generation int, dryRun bool, t SweepTargets) (int, error) {
	deleted := 0
	for _, rule := range subObjectCascades {
		if rule.gate != nil && !rule.gate(t) {
			continue
		}
		refs, err := staleParentRefs(ctx, gs, staleKindFilter(rule.parent, repo, branch, generation), "node_key")
		if err != nil {
			return deleted, err
		}
		n, err := cascadeByField(ctx, gs, rule.child, rule.childField, refs, dryRun)
		if err != nil {
			return deleted, err
		}
		deleted += n
	}
	return deleted, nil
}

// cascadeByField deletes every point of kind whose field is one of refs
// (chunked to the store's MatchAny ceiling), respecting dryRun like every
// other sweep. It is the shared half of the notes cascade (parent_key) and the
// sub-object cascades (mr_key, vulnerability_id).
func cascadeByField(ctx context.Context, gs sweepStore, kind Kind, field string, refs []string, dryRun bool) (int, error) {
	if len(refs) == 0 {
		return 0, nil
	}
	deleted := 0
	for start := 0; start < len(refs); start += noteParentChunk {
		end := min(start+noteParentChunk, len(refs))
		flt := &qdrant.Filter{Must: []*qdrant.Condition{
			qdrant.NewMatch("kind", string(kind)),
			qdrant.NewMatchKeywords(field, refs[start:end]...),
		}}
		n, err := gs.CountByFilter(ctx, flt)
		if err != nil {
			return deleted, fmt.Errorf("graph gc: count %s: %w", kind, err)
		}
		if n == 0 {
			continue
		}
		if dryRun {
			slog.Info("gc would-delete",
				"kind", kind, "field", field, "refs_in", end-start, "points", n)
			deleted += int(n)
			continue
		}
		if err := gs.DeleteByFilter(ctx, flt); err != nil {
			return deleted, fmt.Errorf("graph gc: delete %s: %w", kind, err)
		}
		slog.Info("gc deleted",
			"kind", kind, "field", field, "refs_in", end-start, "points", n)
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

// staleInstanceKindFilter selects one kind's points of the whole instance with
// generation < current — no repo/branch scope: the ownership kinds are
// instance-level and carry repo=""/branch="", so the per-repo filter never
// matches them.
func staleInstanceKindFilter(kind Kind, generation int) *qdrant.Filter {
	return &qdrant.Filter{Must: []*qdrant.Condition{
		qdrant.NewMatch("kind", string(kind)),
		qdrant.NewRange("generation", &qdrant.Range{Lt: qdrant.PtrOf(float64(generation))}),
	}}
}
