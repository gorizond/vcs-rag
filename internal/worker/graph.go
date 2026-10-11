package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
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
	// T008 migration (WORKER_GRAPH_SCHEMA_VER 1→2): the uuid5 namespace is
	// folded with the schema version (graph.Namespace), so the normal per-pass
	// re-scan below already rewrites every point under fresh IDs carrying the
	// canonical keys (node_key/from_key/to_key) — no manual backfill hook and
	// no set_payload pass are needed. A set_payload enrichment of the legacy
	// points would be wasted work: they are re-keyed, not patched, and the
	// first completed pass of a repo+branch deletes every stale generation of
	// it (Sweep, T016) including the whole old namespace. Until that pass,
	// canonical-key path/neighbors over the repo simply resolve nothing.
	p := newPacer(cfg.Worker.GraphInterval)
	// T129: a random start U[0, 2s] avoids a boot herd across lanes.
	if err := p.sleep(ctx, p.startDelay()); err != nil {
		return nil
	}
	// T015: a rotating window start — with more repositories than the per-pass
	// cap, consecutive passes still cover every repository over time.
	start := 0
	// T048: the security-domain availability is probed once per lane and
	// latches off on 403/404, never failing the pass.
	vuln := &vulnGate{}
	for {
		sum := graphPass(ctx, cfg, deps, snap, gs, start, vuln)
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
func graphPass(ctx context.Context, cfg *config.Config, deps RepoDeps, snap *snapshot, gs *store.GraphStore, start int, vuln *vulnGate) *summary {
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
				written, err := processGraphRepoPass(ctx, cfg, deps, gs, repo, generation, vuln)
				if err == nil {
					sum.addUpdated(written)
				}
				return err
			})
		}))
	}
	return sum
}

// processGraphRepo is the entry of the refresh composition: an explicit
// one-repo refresh probes the security domain on its own (a fresh gate), while
// the graph lane calls processGraphRepoPass with its lane-wide gate.
func processGraphRepo(ctx context.Context, cfg *config.Config, deps RepoDeps, gs *store.GraphStore, repo provider.Repo, generation int) (int, error) {
	return processGraphRepoPass(ctx, cfg, deps, gs, repo, generation, &vulnGate{})
}

// processGraphRepoPass extracts the graph of one repository's default branch
// (T015): mirror → branch snapshot → tree → blob → chunk.Extract → writer. A
// completed scan writes the coverage marker (T057) and only then sweeps the
// stale generations of that repo+branch (T016) — a failed or canceled pass
// never sweeps. CPU-only: no embeddings, no commit budget (R13). Lock-free by
// design: the lane and the refresh composition hold the per-repo lock.
func processGraphRepoPass(ctx context.Context, cfg *config.Config, deps RepoDeps, gs *store.GraphStore, repo provider.Repo, generation int, vuln *vulnGate) (int, error) {
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
	// US2/T025: file diffs of the repository's change requests. A provider
	// failure fails the repository pass before its coverage marker and sweep
	// (T016: a failed pass never sweeps); processSafely isolates it (FR-014).
	diffPoints, err := mrFilePoints(ctx, deps, repo, meta, lg)
	if err != nil {
		return written, err
	}
	if len(diffPoints) > 0 {
		if err := gs.Upsert(ctx, diffPoints); err != nil {
			return written, fmt.Errorf("mr_file write: %w", err)
		}
	}
	written += len(diffPoints)
	// US4/T044: ci points of the recent pipelines (success and failure),
	// deployments and environments. Provider failures are best-effort inside
	// ciPoints — they never fail the pass; only a store write error does.
	ciPts := ciPoints(ctx, deps, repo, meta, lg)
	if len(ciPts) > 0 {
		if err := gs.Upsert(ctx, ciPts); err != nil {
			return written, fmt.Errorf("ci write: %w", err)
		}
		written += len(ciPts)
	}
	// US6/T048: security findings behind the lane-wide availability gate; an
	// unavailable domain is skipped (logged once), never failing the pass.
	vulnPts := vuln.ingest(ctx, deps, repo, meta, lg)
	if len(vulnPts) > 0 {
		if err := gs.Upsert(ctx, vulnPts); err != nil {
			return written, fmt.Errorf("vuln write: %w", err)
		}
		written += len(vulnPts)
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

// mrFilePoints builds the mr_file points of one repository (US2/T025): every
// MR/PR and the diffs of its files, read through the optional
// ChangeRequestDiffProvider seam. A provider without diff support yields nil —
// the phase is skipped. Deterministic IDs make the re-upsert each pass
// idempotent and keep the points of the current generation alive for the sweep.
// ponytail: diffs are re-fetched every pass; add an updated_at sidecar if the
// provider API cost of unchanged MRs matters.
func mrFilePoints(ctx context.Context, deps RepoDeps, repo provider.Repo, meta graph.Meta, lg *slog.Logger) ([]*qdrant.PointStruct, error) {
	diffProvider, ok := deps.VCS.(provider.ChangeRequestDiffProvider)
	if !ok {
		return nil, nil
	}
	crs, err := deps.VCS.ListChangeRequests(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("MR/PR list: %w", err)
	}
	var points []*qdrant.PointStruct
	for _, cr := range crs {
		files, err := diffProvider.ListChangeRequestDiffs(ctx, repo, int(cr.ID))
		if err != nil {
			return nil, fmt.Errorf("MR/PR %d diffs: %w", cr.ID, err)
		}
		for _, f := range files {
			// author is owned by the metadata lane (US3): the normalized
			// ChangeRequest carries no author yet.
			points = append(points, graph.MrFilePoint(meta, int(cr.ID), f, "", cr.State))
		}
	}
	if len(points) > 0 {
		lg.Info("graph: mr_file diffs fetched", "mrs", len(crs), "files", len(points))
	}
	return points, nil
}

// ciPoints builds the ci points of one repository (US4/T044): the jobs of the
// recent pipelines — successful and failed alike — plus deployments and
// environments, read through the optional CIPipelineDetailProvider seam. A
// provider without CI-detail support yields nil. Best-effort by design: a
// provider error is logged and the rest of the pass continues (the metadata
// lane's failed-jobs vector path is untouched). Deterministic IDs make the
// re-upsert each pass idempotent and keep the points of the current generation
// alive for the sweep.
// ponytail: pipelines/jobs are re-fetched every pass; add an updated_at
// sidecar if the provider API cost of unchanged pipelines matters.
func ciPoints(ctx context.Context, deps RepoDeps, repo provider.Repo, meta graph.Meta, lg *slog.Logger) []*qdrant.PointStruct {
	detail, ok := deps.VCS.(provider.CIPipelineDetailProvider)
	if !ok {
		return nil
	}
	var points []*qdrant.PointStruct
	pipelines, err := deps.VCS.ListPipelines(ctx, repo)
	if err != nil {
		if ctx.Err() == nil {
			lg.Warn("graph: pipeline list failed, ci jobs skipped", "err", err)
		}
	} else {
		for _, p := range pipelines {
			if ctx.Err() != nil {
				return points
			}
			pid, err := strconv.Atoi(p.ID)
			if err != nil {
				continue // non-numeric provider pipeline ID: not addressable
			}
			jobs, err := detail.ListPipelineJobs(ctx, repo, pid)
			if err != nil {
				lg.Warn("graph: pipeline jobs failed, pipeline skipped", "pipeline", p.ID, "err", err)
				continue
			}
			for _, j := range jobs {
				points = append(points, graph.CiPoint(meta, graph.CiLevelJob, p.ID, j.JobName,
					j.Stage, j.Status, j.DurationMS, j.Ref, j.Env, j.CommitSHA))
			}
		}
	}
	deployments, err := detail.ListDeployments(ctx, repo)
	if err != nil {
		if ctx.Err() == nil {
			lg.Warn("graph: deployments failed, skipped", "err", err)
		}
	} else {
		for _, d := range deployments {
			points = append(points, graph.CiPoint(meta, graph.CiLevelDeployment, strconv.Itoa(d.ID),
				"", "", d.Status, 0, d.Ref, d.Environment, d.CommitSHA))
		}
	}
	envs, err := detail.ListEnvironments(ctx, repo)
	if err != nil {
		if ctx.Err() == nil {
			lg.Warn("graph: environments failed, skipped", "err", err)
		}
	} else {
		for _, e := range envs {
			points = append(points, graph.CiPoint(meta, graph.CiLevelEnvironment, "", e.Name,
				"", e.State, 0, "", e.Name, ""))
		}
	}
	return points
}

// vulnGate latches the availability of the security domain for one graph lane
// (US6/T048). The first call is the probe itself: a typed unavailable
// (403/404, missing scope) turns the domain off for the process and is logged
// once; transient provider errors are logged and left to the next repo/pass.
// ponytail: a per-repo 404 (dependabot/code-scanning off on one repo) also
// latches the whole lane off; a per-repo probe would be finer.
type vulnGate struct {
	mu  sync.Mutex
	off bool
}

// ingest reads the vulnerabilities of one repository and returns their points.
// It returns nil when the provider has no security support, the domain is off,
// the call failed or there are no findings — never an error.
func (g *vulnGate) ingest(ctx context.Context, deps RepoDeps, repo provider.Repo, meta graph.Meta, lg *slog.Logger) []*qdrant.PointStruct {
	vp, ok := deps.VCS.(provider.VulnerabilityProvider)
	if !ok {
		return nil
	}
	g.mu.Lock()
	off := g.off
	g.mu.Unlock()
	if off {
		return nil
	}
	findings, err := vp.ListVulnerabilities(ctx, repo)
	if err != nil {
		if errors.Is(err, provider.ErrVulnerabilitiesUnavailable) {
			g.mu.Lock()
			g.off = true
			g.mu.Unlock()
			lg.Info("graph: security domain unavailable, vuln ingest off", "err", err)
			return nil
		}
		if ctx.Err() == nil {
			lg.Warn("graph: vulnerabilities fetch failed, repo skipped", "err", err)
		}
		return nil
	}
	points := make([]*qdrant.PointStruct, 0, len(findings))
	for _, f := range findings {
		points = append(points, graph.VulnPoint(meta, f.ID, f.Severity, f.Scanner, f.State, f.LocationFile))
	}
	if len(points) > 0 {
		lg.Info("graph: vuln findings fetched", "findings", len(points))
	}
	return points
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
