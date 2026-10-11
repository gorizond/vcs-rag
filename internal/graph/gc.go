package graph

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/qdrant/go-client/qdrant"

	"github.com/gorizond/vcs-rag/internal/store"
)

// Sweep deletes stale-generation points of one repository+branch, one kind at
// a time (mark-and-sweep, T011), and returns how many points were deleted (or,
// in dry-run, would be). A point is stale when its payload generation is lower
// than the current one; points of the current generation are never touched.
//
// dryRun comes from WORKER_GC_DRY_RUN (config.Worker.GCDryRun): when set, only
// a "gc would-delete" log line is written and nothing is deleted.
//
// The caller MUST call Sweep only for a repo+branch whose graph pass has
// completed successfully and is not in flight: a failed or running pass may
// still write points of the previous generation, which Sweep would delete
// mid-pass (the graph lane owns that guarantee).
func Sweep(ctx context.Context, gs *store.GraphStore, repo, branch string, generation int, dryRun bool) (deleted int, err error) {
	for _, kind := range []Kind{KindNode, KindEdge, KindMrFile, KindCi, KindVuln} {
		flt := staleKindFilter(kind, repo, branch, generation)
		n, err := gs.CountByFilter(ctx, flt)
		if err != nil {
			return deleted, fmt.Errorf("graph gc: count %s: %w", kind, err)
		}
		if n == 0 {
			continue
		}
		if dryRun {
			slog.Info("gc would-delete",
				"kind", kind, "repo", repo, "branch", branch,
				"generation_below", generation, "points", n)
			deleted += int(n)
			continue
		}
		if err := gs.DeleteByFilter(ctx, flt); err != nil {
			return deleted, fmt.Errorf("graph gc: delete %s: %w", kind, err)
		}
		slog.Info("gc deleted",
			"kind", kind, "repo", repo, "branch", branch,
			"generation_below", generation, "points", n)
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
