package worker

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/qdrant/go-client/qdrant"

	"github.com/gorizond/vcs-rag/internal/chunk"
	"github.com/gorizond/vcs-rag/internal/config"
	"github.com/gorizond/vcs-rag/internal/gitengine"
	"github.com/gorizond/vcs-rag/internal/graph"
	"github.com/gorizond/vcs-rag/internal/log"
	"github.com/gorizond/vcs-rag/internal/provider"
	"github.com/gorizond/vcs-rag/internal/store"
)

// runGraph is the SDLC graph lane (T010/R13): an adaptive paced loop over the
// discovery registry — ceiling WORKER_GRAPH_INTERVAL, floor the shared
// paceMin. CPU-only by design: it never touches the embeddings limiter nor the
// per-pass commit budget of the index lane.
func runGraph(ctx context.Context, cfg *config.Config, deps RepoDeps, snap *snapshot, gs *store.GraphStore) error {
	deps.Logger = log.WithPhase(deps.Logger, "graph")
	// T006/T007: bootstrap once at lane start. The vector-less gate is
	// fail-hard for the LANE, not for the worker — a rejected collection
	// disables the graph with one prominent ERROR instead of crashing Run.
	if err := gs.EnsureGraphCollection(ctx); err != nil {
		deps.Logger.Error("graph lane disabled: graph collection bootstrap failed",
			"collection", cfg.Qdrant.GraphCollection, "err", err)
		return nil
	}
	if err := gs.EnsureGraphIndexes(ctx); err != nil {
		deps.Logger.Error("graph lane disabled: graph payload-index bootstrap failed",
			"collection", cfg.Qdrant.GraphCollection, "err", err)
		return nil
	}
	p := newPacer(cfg.Worker.GraphInterval)
	// T129: a random start U[0, 2s] avoids a boot herd across lanes.
	if err := p.sleep(ctx, p.startDelay()); err != nil {
		return nil
	}
	// T015: a rotating window start — with more repositories than the per-pass
	// cap, consecutive passes still cover every repository over time.
	start := 0
	for {
		sum := graphPass(ctx, cfg, deps, snap, gs, start)
		start += int(sum.repos.Load())
		// Written points are the real-work signal: with none the lane backs
		// off to its ceiling instead of spinning.
		busy := sum.updated.Load() > 0
		delay := p.next(busy, false, 0)
		// Graph-specific pass line: the shared summary log has no
		// "points_written" field and summary.go is owned by another task.
		deps.Logger.Info("pass summary: graph",
			"repos_scanned", sum.repos.Load(),
			"points_written", sum.updated.Load(),
			"errors", sum.failed.Load(),
			"pace_delay", delay.Round(time.Millisecond).String(),
			"pace_reason", p.reason,
			"duration", time.Since(sum.start).Round(time.Millisecond).String(),
		)
		if err := p.sleep(ctx, delay); err != nil {
			return nil
		}
	}
}

// graphPass extracts the graph of up to WORKER_GRAPH_MAX_REPOS_PER_PASS
// repositories from the discovery snapshot, starting at the rotating offset
// (T015): per-repo lock, failure isolation (FR-014). One generation stamp is
// shared by every repository of the pass — the sweep of a repository (T016)
// compares against exactly the generation of its scan.
func graphPass(ctx context.Context, cfg *config.Config, deps RepoDeps, snap *snapshot, gs *store.GraphStore, start int) *summary {
	sum := newSummary("graph")
	repos := snap.get()
	if len(repos) == 0 {
		return sum
	}
	if start > 0 {
		start %= len(repos)
		repos = repos[start:]
	}
	if max := cfg.Worker.GraphMaxReposPerPass; max > 0 && len(repos) > max {
		repos = repos[:max]
	}
	generation := int(time.Now().Unix())
	for _, repo := range repos {
		if ctx.Err() != nil {
			break
		}
		sum.countRepo(processSafely(ctx, deps.Logger, repo, "graph", func() error {
			// T110: the per-repo lock is taken at the call site only.
			return withRepoLock(repoSlug(repo.Namespace+"/"+repo.Path), func() error {
				written, err := processGraphRepo(ctx, cfg, deps, gs, repo, generation)
				if err == nil {
					sum.addUpdated(written)
				}
				return err
			})
		}))
	}
	return sum
}

// processGraphRepo extracts the graph of one repository's default branch
// (T015): mirror → branch snapshot → tree → blob → chunk.Extract → writer. A
// completed scan writes the coverage marker (T057) and only then sweeps the
// stale generations of that repo+branch (T016) — a failed or canceled pass
// never sweeps. CPU-only: no embeddings, no commit budget (R13). Lock-free by
// design: the lane and the refresh composition hold the per-repo lock.
func processGraphRepo(ctx context.Context, cfg *config.Config, deps RepoDeps, gs *store.GraphStore, repo provider.Repo, generation int) (int, error) {
	lg := log.WithRepo(deps.Logger, repo.Namespace+"/"+repo.Path)
	mirror, err := deps.Engine.EnsureLocalMirror(ctx, deps.VCSType, repo)
	if err != nil {
		return 0, fmt.Errorf("mirror: %w", err)
	}
	refs, err := deps.Engine.Snapshot(ctx, mirror)
	if err != nil {
		return 0, fmt.Errorf("branch snapshot: %w", err)
	}
	branch := repo.DefaultBranch
	if branch == "" {
		branch = deps.Engine.HeadBranch(ctx, mirror)
	}
	tip, ok := refs[branch]
	if branch == "" || !ok {
		lg.Debug("graph: default branch is not in the mirror", "branch", branch)
		return 0, nil
	}
	files, err := deps.Engine.TreeFiles(ctx, mirror, tip, cfg.Worker.MaxFileBytes)
	if err != nil {
		return 0, fmt.Errorf("tree: %w", err)
	}
	reader, err := deps.Engine.NewBlobReader(ctx, mirror)
	if err != nil {
		return 0, err
	}
	defer reader.Close()

	meta := graph.Meta{
		Repo:       repo.Namespace + "/" + repo.Path,
		GroupOrg:   repo.Namespace,
		VCSType:    deps.VCSType,
		Branch:     branch,
		CommitSHA:  tip,
		Language:   "go",
		SchemaVer:  cfg.Worker.GraphSchemaVer,
		Generation: generation,
	}
	modPath := repoModulePath(reader, files)
	scanned, err := scanGoFiles(ctx, files, reader, lg)
	if err != nil {
		return 0, err
	}
	written, err := graph.WriteGraph(ctx, gs, scanned, meta, func(p string) bool {
		return modPath == "" || !strings.HasPrefix(p, modPath)
	})
	if err != nil {
		return 0, fmt.Errorf("graph write: %w", err)
	}
	// T057: the coverage marker of the completed scan, upserted again every
	// pass (deterministic ID, last writer wins).
	if err := gs.Upsert(ctx, []*qdrant.PointStruct{graph.CoveragePoint(meta, tip)}); err != nil {
		return written, fmt.Errorf("coverage: %w", err)
	}
	written++
	if ctx.Err() != nil {
		return written, nil // a canceled pass is never swept (T016)
	}
	deleted, err := graph.Sweep(ctx, gs, meta.Repo, branch, generation, cfg.Worker.GCDryRun)
	if err != nil {
		return written, fmt.Errorf("sweep: %w", err)
	}
	lg.Info("graph scan done", "branch", branch, "files", len(scanned), "points", written, "swept", deleted)
	return written, nil
}

// repoModulePath reads the root go.mod module path of the scanned tree ("" —
// the repository has none) once per scan: the external-import classifier.
func repoModulePath(reader *gitengine.BlobReader, files []gitengine.TreeFile) string {
	for _, f := range files {
		if f.Path != "go.mod" {
			continue
		}
		content, found, err := reader.ReadBlob(f.SHA)
		if err != nil || !found {
			return ""
		}
		return parseModulePath(content)
	}
	return ""
}

// parseModulePath extracts the path of the go.mod "module" directive ("" —
// unknown); the caller then treats every import as external. The directive is
// single-line by definition; quoted paths are unquoted.
func parseModulePath(content []byte) string {
	for _, line := range strings.Split(string(content), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "module" {
			return strings.Trim(fields[1], "\"`")
		}
	}
	return ""
}

// scanGoFiles reads and parses the .go blobs of one tree; an unparsable file is
// skipped (debug), not fatal — its neighbors still contribute to the graph.
func scanGoFiles(ctx context.Context, files []gitengine.TreeFile, reader *gitengine.BlobReader, lg *slog.Logger) ([]graph.FileGraph, error) {
	out := make([]graph.FileGraph, 0, len(files))
	for _, f := range files {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !strings.HasSuffix(f.Path, ".go") {
			continue
		}
		content, found, err := reader.ReadBlob(f.SHA)
		if err != nil {
			return nil, fmt.Errorf("blob %s: %w", f.Path, err)
		}
		if !found || gitengine.IsBinary(content) {
			continue
		}
		defs, imports, err := chunk.Extract(f.Path, content)
		if err != nil {
			lg.Debug("graph: unparsable file skipped", "file", f.Path, "err", err)
			continue
		}
		pkg := ""
		if len(defs) > 0 {
			pkg = defs[0].Package
		}
		out = append(out, graph.FileGraph{Path: f.Path, Package: pkg, Defs: defs, Imports: imports})
	}
	return out, nil
}
