package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path"
	"sort"
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
	// latches off on 403/404, never failing the pass. T037: the runner domain
	// (conditional by API/role) latches off the same way.
	vuln := &vulnGate{}
	runners := &runnerGate{}
	for {
		sum := graphPass(ctx, cfg, deps, snap, gs, start, vuln, runners)
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
func graphPass(ctx context.Context, cfg *config.Config, deps RepoDeps, snap *snapshot, gs *store.GraphStore, start int, vuln *vulnGate, runners *runnerGate) *summary {
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
				written, err := processGraphRepoPass(ctx, cfg, deps, gs, repo, generation, vuln, runners)
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
	return processGraphRepoPass(ctx, cfg, deps, gs, repo, generation, &vulnGate{}, &runnerGate{})
}

// processGraphRepoPass extracts the graph of one repository's default branch
// (T015): mirror → branch snapshot → tree → blob → chunk.Extract → writer. A
// completed scan writes the coverage marker (T057) and only then sweeps the
// stale generations of that repo+branch (T016) — a failed or canceled pass
// never sweeps. The tree/branch structure (US3) and the group/project
// ownership chain (US4) follow the code graph; then the change-request list
// feeds both the mr_file diffs and the change-request notes, and the orbit
// phases (US2/US3) add the planning nodes (work items, milestones, labels) and
// the users/notes. The runner nodes (US4) come behind their lane-wide gate
// last. Every orbit and schema-v3 domain is best-effort: a failed or
// unavailable listing withholds only that domain's sweep (SweepTargets) and
// never the pass. CPU-only: no embeddings, no commit budget (R13). Lock-free
// by design: the lane and the refresh composition hold the per-repo lock.
func processGraphRepoPass(ctx context.Context, cfg *config.Config, deps RepoDeps, gs *store.GraphStore, repo provider.Repo, generation int, vuln *vulnGate, runners *runnerGate) (int, error) {
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
	// The sweep gates of this pass: a family's flag is set only by a completed
	// listing, so a failed or unavailable domain keeps its stale points
	// instead of deleting them blindly (FR-019/FR-020).
	var targets graph.SweepTargets
	// US3/T034: the branch nodes and the tree relations of the pass tree. One
	// best-effort provider listing (TreeReader) yields a branch node per branch
	// and the project→branch contains edge; the default branch also gets the
	// branch→root-directory contains and the on_branch edges of every scanned
	// file and directory (the pass-branch half is already written by the
	// code-graph step; deterministic IDs keep the overlap idempotent). A failed
	// or unavailable listing is logged and withholds only the branch sweep
	// (targets.Structure), never the pass (FR-024).
	structurePts, structureOK := structurePoints(ctx, deps, repo, meta, scanned, lg)
	if len(structurePts) > 0 {
		if err := gs.Upsert(ctx, structurePts); err != nil {
			return written, fmt.Errorf("structure write: %w", err)
		}
		written += len(structurePts)
	}
	targets.Structure = structureOK
	host := vcsHost(cfg, deps.VCSType)
	// US4/T039: the group/project ownership chain. The group listing and the
	// per-group project listings are best-effort: a failed one is logged and
	// withholds the ownership sweep (targets.Ownership), never the pass
	// (FR-024).
	ownershipPts, ownershipOK := ownershipPoints(ctx, deps, meta, host, lg)
	if len(ownershipPts) > 0 {
		if err := gs.Upsert(ctx, ownershipPts); err != nil {
			return written, fmt.Errorf("ownership write: %w", err)
		}
		written += len(ownershipPts)
	}
	targets.Ownership = ownershipOK
	// US2/T025: file diffs of the repository's change requests. A provider
	// failure fails the repository pass before its coverage marker and sweep
	// (T016: a failed pass never sweeps); processSafely isolates it (FR-014).
	// The list is shared with the change-request notes of the orbit phase.
	crs, err := deps.VCS.ListChangeRequests(ctx, repo)
	if err != nil {
		return written, fmt.Errorf("MR/PR list: %w", err)
	}
	diffPoints, err := mrFilePoints(ctx, deps, repo, meta, crs, lg)
	if err != nil {
		return written, err
	}
	if len(diffPoints) > 0 {
		if err := gs.Upsert(ctx, diffPoints); err != nil {
			return written, fmt.Errorf("mr_file write: %w", err)
		}
	}
	written += len(diffPoints)
	// US2/T025: the review-family nodes, right after their file diffs. One
	// listing of requests and their diff versions yields the merge_request /
	// merge_request_diff nodes and the in_project/has_diff/has_latest_diff
	// relations; the has_file and merged/reviewer joins are written only
	// against endpoints that resolve (skipped, logged, otherwise). Best-effort
	// by design: a failed listing is logged and the pass continues — the
	// points the earlier phases already wrote are never rolled back (FR-024).
	// Only a completed request listing (and every per-request diff listing)
	// sets targets.Review, so a withheld listing keeps its stale points. A
	// store write error, unlike a provider one, fails the pass.
	mrPts, reviewOK := reviewPoints(ctx, deps, repo, meta, diffPoints, lg)
	if len(mrPts) > 0 {
		if err := gs.Upsert(ctx, mrPts); err != nil {
			return written, fmt.Errorf("merge request write: %w", err)
		}
		written += len(mrPts)
	}
	targets.Review = reviewOK
	// US4/T044: ci points of the recent pipelines (success and failure),
	// deployments and environments. Provider failures are best-effort inside
	// ciPoints — they never fail the pass; only a store write error does. Only
	// a completed pipeline listing (and every per-pipeline job, deployment and
	// environment listing) sets targets.CI, so a withheld listing keeps the
	// stale ci points instead of deleting them blindly.
	ciPts, ciOK := ciPoints(ctx, deps, repo, meta, lg)
	if len(ciPts) > 0 {
		if err := gs.Upsert(ctx, ciPts); err != nil {
			return written, fmt.Errorf("ci write: %w", err)
		}
		written += len(ciPts)
	}
	targets.CI = ciOK
	// US6/T048: security findings behind the lane-wide availability gate; an
	// unavailable domain is skipped (logged once), never failing the pass. Only
	// a completed listing sets targets.Vuln, so the legacy vuln points of a
	// latched-off or failed domain are kept instead of swept blindly.
	vulnPts, vulnOK := vuln.ingest(ctx, deps, repo, meta, lg)
	if len(vulnPts) > 0 {
		if err := gs.Upsert(ctx, vulnPts); err != nil {
			return written, fmt.Errorf("vuln write: %w", err)
		}
		written += len(vulnPts)
	}
	targets.Vuln = vulnOK
	// US3/T031 + US2/T023: the orbit nodes — work items, milestones, labels,
	// then the users and their notes/comments. Each domain is best-effort and
	// idempotent (deterministic IDs); the targets tell Sweep which kinds saw a
	// complete listing and may be generation-swept.
	planning, items, orbitTargets := workItemPoints(ctx, deps, repo, meta, host, lg)
	targets.WorkItems = orbitTargets.WorkItems
	targets.Milestones = orbitTargets.Milestones
	targets.Labels = orbitTargets.Labels
	people, userSweep := userNotePoints(ctx, deps, repo, meta, host, items, crs, targets.WorkItems, lg)
	targets.Users = userSweep
	if orbitPts := append(planning, people...); len(orbitPts) > 0 {
		if err := gs.Upsert(ctx, orbitPts); err != nil {
			return written, fmt.Errorf("orbit write: %w", err)
		}
		written += len(orbitPts)
	}
	// US1/T018: the schema-v3 security sub-family, after the orbit block. One
	// runtime-gated findings listing carries the vulnerability, finding,
	// occurrence, security_scan, vulnerability_scanner and identifier nodes
	// plus the relations between them. A provider without the SecurityVulnReader
	// seam or a typed unavailable (403/404, missing tier/scope) is skipped —
	// logged, never a pass failure, never a GAP (FR-024). Only a completed
	// listing sets targets.Security, so a withheld domain keeps its stale
	// points instead of deleting them blindly.
	secPts, securityOK := securityPoints(ctx, deps, repo, meta, host, ciPipelineKeys(ciPts), lg)
	if len(secPts) > 0 {
		if err := gs.Upsert(ctx, secPts); err != nil {
			return written, fmt.Errorf("security write: %w", err)
		}
		written += len(secPts)
	}
	targets.Security = securityOK
	// US4/T037: the runner nodes behind the lane-wide availability gate. The
	// domain is conditional by design (GitLab needs Maintainer+; GitHub needs
	// Actions read) — an unavailable provider is skipped, logged once, and
	// never fails the pass; only its sweep stays withheld. A store write error
	// is a graph-store problem, not a provider one, and fails the pass.
	runnerPts, runnersOK := runners.ingest(ctx, deps, repo, meta, host, lg)
	if len(runnerPts) > 0 {
		if err := gs.Upsert(ctx, runnerPts); err != nil {
			return written, fmt.Errorf("runner write: %w", err)
		}
		written += len(runnerPts)
	}
	targets.Runners = runnersOK
	// T057: the coverage marker of the completed scan, upserted again every
	// pass (deterministic ID, last writer wins).
	if err := gs.Upsert(ctx, []*qdrant.PointStruct{graph.CoveragePoint(meta, tip)}); err != nil {
		return written, fmt.Errorf("coverage: %w", err)
	}
	written++
	if ctx.Err() != nil {
		return written, nil // a canceled pass is never swept (T016)
	}
	deleted, err := graph.Sweep(ctx, gs, meta.Repo, branch, generation, cfg.Worker.GCDryRun, targets)
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
func mrFilePoints(ctx context.Context, deps RepoDeps, repo provider.Repo, meta graph.Meta, crs []provider.ChangeRequest, lg *slog.Logger) ([]*qdrant.PointStruct, error) {
	diffProvider, ok := deps.VCS.(provider.ChangeRequestDiffProvider)
	if !ok {
		return nil, nil
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

// mergeRequestPoints is reviewPoints without the listing-completeness flag —
// the builder seam the unit tests exercise directly.
func mergeRequestPoints(ctx context.Context, deps RepoDeps, repo provider.Repo, meta graph.Meta, diffPoints []*qdrant.PointStruct, lg *slog.Logger) []*qdrant.PointStruct {
	points, _ := reviewPoints(ctx, deps, repo, meta, diffPoints, lg)
	return points
}

// reviewPoints builds the review-family nodes of one repository (US2/T025):
// the merge_request node of every change request and the merge_request_diff
// node of each of its diff versions, plus their in_project, has_diff and
// has_latest_diff relations. The has_file and merged/reviewer joins are
// written only against an endpoint that resolves to a canonical node_key; the
// legacy mr_file points and the username-only people fields do not resolve, so
// those edges are skipped (counted for the debug line) rather than left
// dangling. Best-effort by design (FR-024): a provider without the
// MergeRequestNodeReader seam or a failed listing of one request is logged and
// skipped — the rest of the pass, and the points it already wrote, stay
// intact; only a store write error fails the pass. The returned flag reports
// whether every listing of the family completed (the request listing and each
// request's diff-version listing): only then may Sweep generation-sweep the
// review kinds (targets.Review), so a failed listing keeps its stale points.
// Deterministic IDs make the re-upsert each pass idempotent and keep the
// points of the current generation alive for the sweep.
// ponytail: the diff versions of every request are re-fetched each pass; add
// an updated_at sidecar if the provider API cost of unchanged MRs matters.
func reviewPoints(ctx context.Context, deps RepoDeps, repo provider.Repo, meta graph.Meta, diffPoints []*qdrant.PointStruct, lg *slog.Logger) ([]*qdrant.PointStruct, bool) {
	reader, ok := deps.VCS.(provider.MergeRequestNodeReader)
	if !ok {
		return nil, false
	}
	reqs, err := reader.ListMergeRequestNodes(ctx, repo)
	if err != nil {
		orbitFail(ctx, lg, "merge request nodes", err)
		return nil, false
	}
	complete := true
	fileKeys, resolvedFiles := mrFileKeyIndex(diffPoints)
	projectKey := graph.RefProject(meta.Repo)
	var (
		points        []*qdrant.PointStruct
		skippedPeople int
	)
	for _, cr := range reqs {
		if ctx.Err() != nil {
			break
		}
		iid := strconv.FormatInt(cr.ID, 10)
		mrKey := graph.RefMergeRequest(meta.Repo, iid)
		// The author_key stays empty, review.go's unresolved case: the request
		// carries a username, not the provider id RefUser keys on.
		points = append(points, graph.MergeRequestPoint(meta, cr, ""))
		points = append(points, graph.InProjectEdge(meta, mrKey, "merge_request", projectKey))
		// merged/reviewer are user joins and need a canonical RefUser
		// endpoint: the request carries reviewer/author usernames while user
		// nodes key on the numeric provider id, no username→id lookup exists
		// in the pass, and the merger identity is not surfaced at all — the
		// joins are skipped (counted) instead of left dangling. Merge them
		// once a resolved-user seam lands on the request. ponytail: the debug
		// counter is the signal; drop it with the seam.
		skippedPeople += len(cr.Reviewers)
		if cr.Merged {
			skippedPeople++
		}
		refs, err := reader.ListMergeRequestDiffRefs(ctx, repo, int(cr.ID))
		if err != nil {
			orbitFail(ctx, lg, "merge request diff refs", err)
			complete = false
			continue
		}
		tip := latestDiffIndex(refs, cr.MergeCommitSHA)
		for i, d := range refs {
			dp := graph.MergeRequestDiffPoint(meta, int(cr.ID), d)
			// The canonical key of a diff point is its own node_key; the
			// edge targets are read from the built point so the fallback of
			// RefMergeRequestDiff (head_sha, else version) stays single-sourced.
			diffKey := dp.GetPayload()["node_key"].GetStringValue()
			points = append(points, dp)
			points = append(points, graph.HasDiffEdge(meta, mrKey, diffKey))
			points = append(points, graph.InProjectEdge(meta, diffKey, "merge_request_diff", projectKey))
			if i != tip {
				continue
			}
			points = append(points, graph.HasLatestDiffEdge(meta, mrKey, diffKey))
			// has_file may only target a diff-file point that exposes its own
			// canonical node_key: the legacy mr_file points carry none yet, so
			// the edge is skipped rather than fabricated.
			for _, fileKey := range fileKeys[iid] {
				points = append(points, graph.HasFileEdge(meta, diffKey, fileKey))
			}
		}
	}
	if len(points) > 0 {
		lg.Info("graph: merge request nodes fetched", "requests", len(reqs), "points", len(points))
	}
	if skipped := len(diffPoints) - resolvedFiles; skipped > 0 {
		lg.Debug("graph: has_file skipped, diff-file points carry no node_key", "files", skipped)
	}
	if skippedPeople > 0 {
		lg.Debug("graph: merged/reviewer edges skipped, usernames have no user id", "joins", skippedPeople)
	}
	return points, complete
}

// latestDiffIndex picks the tip version of one request's diff list (US2/T025):
// the version whose head_sha equals the request's merge commit — a fast-forward
// merge leaves the merge commit on that head — else the first entry. The
// normalized ChangeRequest carries no dedicated diff head, so the merge commit
// is the only head-like anchor; the fallback relies on the provider listing
// versions newest-first.
func latestDiffIndex(refs []provider.MergeRequestDiffRef, mergeSHA string) int {
	if mergeSHA != "" {
		for i, r := range refs {
			if r.HeadSHA == mergeSHA {
				return i
			}
		}
	}
	return 0
}

// mrFileKeyIndex indexes the canonical node_keys of the diff-file points of one
// pass by request iid. has_file may only target a file point that exposes its
// own node_key — the legacy mr_file points carry none yet — so the index stays
// empty today and the edge is skipped rather than fabricated; resolved is the
// caller's debug signal (the number of indexed, resolvable points).
func mrFileKeyIndex(diffPoints []*qdrant.PointStruct) (map[string][]string, int) {
	keys := make(map[string][]string, len(diffPoints))
	resolved := 0
	for _, p := range diffPoints {
		payload := p.GetPayload()
		key := payload["node_key"].GetStringValue()
		if key == "" {
			continue
		}
		iid := strconv.FormatInt(payload["mr_id"].GetIntegerValue(), 10)
		keys[iid] = append(keys[iid], key)
		resolved++
	}
	return keys, resolved
}

// vcsHost is the provider instance host stamped on the user node keys (RefUser)
// and vcs_host: the configured base URL's host, so a self-hosted instance never
// shares user identity with gitlab.com/github.com. The default GitHub REST
// endpoint is the API host, not the instance — the instance identity is
// github.com (user.go's contract); an unparsable or absent base URL falls back
// to the provider type.
func vcsHost(cfg *config.Config, vcsType string) string {
	if cfg == nil {
		return vcsType
	}
	base := cfg.Provider.GitLabBaseURL
	if vcsType == "github" {
		base = cfg.Provider.GitHubBaseURL
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return vcsType
	}
	if vcsType == "github" && u.Host == "api.github.com" {
		return "github.com"
	}
	return u.Host
}

// orbitFail logs one failed best-effort orbit listing: a typed unavailable
// (403/404, missing scope) is a capability signal logged at info, anything
// else at warn; a canceled context stays silent. Never an error — the only
// cost of a failed domain is that its sweep stays withheld (SweepTargets).
func orbitFail(ctx context.Context, lg *slog.Logger, domain string, err error) {
	if ctx.Err() != nil {
		return
	}
	if errors.Is(err, provider.ErrDomainUnavailable) {
		lg.Info("graph: orbit domain unavailable, skipped", "domain", domain, "err", err)
		return
	}
	lg.Warn("graph: orbit listing failed, skipped", "domain", domain, "err", err)
}

// workItemPoints builds the planning nodes and item edges of one repository
// (US3/T031): the work items (issues/tasks), milestones and labels, each
// through its optional provider seam. A domain whose listing failed is skipped
// and stays unswept (the returned targets); the items feed the users/notes
// phase. The authored/assigned_to edges resolve users by RefUser(host, id), so
// they join UserPoint by construction; a labelled edge is written only for a
// label the successful listing carries (a differently-scoped ref would dangle
// forever). Deterministic IDs make the re-upsert each pass idempotent.
// ponytail: the three listings are re-fetched every pass; add an updated_at
// sidecar if the provider API cost of unchanged planning data matters.
func workItemPoints(ctx context.Context, deps RepoDeps, repo provider.Repo, meta graph.Meta, host string, lg *slog.Logger) ([]*qdrant.PointStruct, []provider.WorkItem, graph.SweepTargets) {
	var (
		pts     []*qdrant.PointStruct
		items   []provider.WorkItem
		targets graph.SweepTargets
	)
	if p, ok := deps.VCS.(provider.WorkItemsProvider); ok {
		listed, err := p.ListWorkItems(ctx, repo)
		if err != nil {
			orbitFail(ctx, lg, "work items", err)
		} else {
			items, targets.WorkItems = listed, true
		}
	}
	if p, ok := deps.VCS.(provider.MilestonesProvider); ok {
		listed, err := p.ListMilestones(ctx, repo)
		if err != nil {
			orbitFail(ctx, lg, "milestones", err)
		} else {
			targets.Milestones = true
			for _, ms := range listed {
				pts = append(pts, graph.MilestonePoint(meta, ms))
			}
		}
	}
	labelScope := map[string]string{}
	if p, ok := deps.VCS.(provider.LabelsProvider); ok {
		listed, err := p.ListLabels(ctx, repo)
		if err != nil {
			orbitFail(ctx, lg, "labels", err)
		} else {
			targets.Labels = true
			for _, l := range listed {
				labelScope[l.Name] = l.Scope
				pts = append(pts, graph.LabelPoint(meta, l))
			}
		}
	}
	for _, item := range items {
		pts = append(pts, graph.WorkItemPoint(meta, item))
		if item.AuthorID != "" {
			pts = append(pts, graph.AuthoredEdge(meta, item, graph.RefUser(host, item.AuthorID)))
		}
		for _, id := range item.AssigneeIDs {
			if id != "" {
				pts = append(pts, graph.AssignedToEdge(meta, item, graph.RefUser(host, id)))
			}
		}
		for _, name := range item.LabelNames {
			scope, ok := labelScope[name]
			if !ok {
				continue // no label node of this name: the edge would dangle
			}
			pts = append(pts, graph.LabelledEdge(meta, item, scope, name))
		}
		if item.MilestoneID != 0 {
			pts = append(pts, graph.InMilestoneEdge(meta, item, strconv.Itoa(item.MilestoneID)))
		}
	}
	return pts, items, targets
}

// userNotePoints builds the user nodes and the note nodes/edges of one
// repository (US2/T023): the comments of every work item and change request,
// then the accounts behind the item authors, assignees and note authors. Users
// are fetched once per pass, deduped by provider id. ok reports whether the
// user set of the pass is complete — work items listed, every listable note
// parent listed, every GetUser succeeded — only then may Sweep
// generation-sweep user nodes, else it would delete accounts a partial pass did
// not re-see. A comment with an empty author id is written without author_key
// and without its edge (ghost account).
// ponytail: comments are re-fetched per parent every pass; an updated_at
// sidecar could skip unchanged ones if the provider API cost matters.
func userNotePoints(ctx context.Context, deps RepoDeps, repo provider.Repo, meta graph.Meta, host string, items []provider.WorkItem, crs []provider.ChangeRequest, itemsOK bool, lg *slog.Logger) ([]*qdrant.PointStruct, bool) {
	notes, notesOK := deps.VCS.(provider.NotesProvider)
	users, usersOK := deps.VCS.(provider.UsersProvider)
	if !notesOK && !usersOK {
		return nil, false
	}
	var pts []*qdrant.PointStruct
	ids := map[string]bool{}
	collect := func(id string) {
		if id != "" {
			ids[id] = true
		}
	}
	for _, item := range items {
		collect(item.AuthorID)
		for _, id := range item.AssigneeIDs {
			collect(id)
		}
	}
	// notesComplete: the note set is enumerable only when every parent was
	// listed; a provider without the notes seam has no notes at all (complete).
	notesComplete := !notesOK
	if notesOK {
		notesComplete = itemsOK // an unlisted work item makes the note set partial
		appendNotes := func(parentKind, parentKey string, parentID int) (stop bool) {
			ns, err := notes.ListNotes(ctx, repo, parentKind, parentID)
			if err != nil {
				orbitFail(ctx, lg, parentKind+" notes", err)
				notesComplete = false
				return errors.Is(err, provider.ErrDomainUnavailable)
			}
			for _, n := range ns {
				key := graph.RefNote(meta.Repo, strconv.FormatInt(n.ID, 10))
				authorKey := ""
				if n.AuthorID != "" {
					authorKey = graph.RefUser(host, n.AuthorID)
					collect(n.AuthorID)
				}
				pts = append(pts, graph.NotePoint(meta, n, parentKey, authorKey))
				if authorKey != "" {
					pts = append(pts, graph.CommentedPoint(meta, key, authorKey))
				}
			}
			return false
		}
		var off bool
		if itemsOK {
			for _, item := range items {
				if ctx.Err() != nil {
					return pts, false
				}
				key := graph.RefWorkItem(meta.Repo, item.ItemType, strconv.Itoa(item.ID))
				if off = appendNotes("issue", key, item.ID); off {
					break
				}
			}
		}
		if !off {
			for _, cr := range crs {
				if ctx.Err() != nil {
					return pts, false
				}
				if appendNotes("change_request", changeRequestParent(meta.Repo, meta.VCSType, int(cr.ID)), int(cr.ID)) {
					break
				}
			}
		}
	}
	userComplete := usersOK && itemsOK && notesComplete
	if usersOK {
		sorted := make([]string, 0, len(ids))
		for id := range ids {
			sorted = append(sorted, id)
		}
		sort.Strings(sorted)
		for _, id := range sorted {
			if ctx.Err() != nil {
				return pts, false
			}
			u, err := users.GetUser(ctx, id)
			if err != nil {
				userComplete = false
				lg.Debug("graph: user fetch failed, node skipped", "user", id, "err", err)
				continue
			}
			pts = append(pts, graph.UserPoint(meta, host, u))
		}
	}
	return pts, userComplete
}

// changeRequestParent is the canonical parent ref of a change request's notes:
// GitLab MRs are not work-item nodes (the issues-only reader, R8), so they get
// the dedicated merge_request discriminator; GitHub PRs ARE work items
// (item_type "issue"), so their notes join the PR's work-item node.
func changeRequestParent(repo, vcsType string, id int) string {
	if vcsType == "github" {
		return graph.RefWorkItem(repo, "issue", strconv.Itoa(id))
	}
	return graph.RefWorkItem(repo, "merge_request", strconv.Itoa(id))
}

// ciPoints builds the ci points of one repository (US4/T044): the jobs of the
// recent pipelines — successful and failed alike — plus deployments and
// environments, read through the optional CIPipelineDetailProvider seam. A
// provider without CI-detail support yields nil,false. Best-effort by design:
// a provider error is logged and the rest of the pass continues (the metadata
// lane's failed-jobs vector path is untouched). ok reports whether every
// listing of the family completed — only then may Sweep generation-sweep ci
// points (targets.CI); a failed listing keeps its stale points. Deterministic
// IDs make the re-upsert each pass idempotent and keep the points of the
// current generation alive for the sweep.
// ponytail: pipelines/jobs are re-fetched every pass; add an updated_at
// sidecar if the provider API cost of unchanged pipelines matters.
func ciPoints(ctx context.Context, deps RepoDeps, repo provider.Repo, meta graph.Meta, lg *slog.Logger) ([]*qdrant.PointStruct, bool) {
	detail, ok := deps.VCS.(provider.CIPipelineDetailProvider)
	if !ok {
		return nil, false
	}
	complete := true
	var points []*qdrant.PointStruct
	pipelines, err := deps.VCS.ListPipelines(ctx, repo)
	if err != nil {
		complete = false
		if ctx.Err() == nil {
			lg.Warn("graph: pipeline list failed, ci jobs skipped", "err", err)
		}
	} else {
		for _, p := range pipelines {
			if ctx.Err() != nil {
				return points, false
			}
			pid, err := strconv.Atoi(p.ID)
			if err != nil {
				continue // non-numeric provider pipeline ID: not addressable
			}
			jobs, err := detail.ListPipelineJobs(ctx, repo, pid)
			if err != nil {
				complete = false
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
		complete = false
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
		complete = false
		if ctx.Err() == nil {
			lg.Warn("graph: environments failed, skipped", "err", err)
		}
	} else {
		for _, e := range envs {
			points = append(points, graph.CiPoint(meta, graph.CiLevelEnvironment, "", e.Name,
				"", e.State, 0, "", e.Name, ""))
		}
	}
	return points, complete
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
// It returns nil,false when the provider has no security support, the domain is
// off or the call failed — never an error. ok reports whether the listing of
// this repo completed: only then may Sweep generation-sweep the legacy vuln
// points (targets.Vuln); a latched-off or failed domain keeps them instead of
// deleting them blindly.
func (g *vulnGate) ingest(ctx context.Context, deps RepoDeps, repo provider.Repo, meta graph.Meta, lg *slog.Logger) ([]*qdrant.PointStruct, bool) {
	vp, ok := deps.VCS.(provider.VulnerabilityProvider)
	if !ok {
		return nil, false
	}
	g.mu.Lock()
	off := g.off
	g.mu.Unlock()
	if off {
		return nil, false
	}
	findings, err := vp.ListVulnerabilities(ctx, repo)
	if err != nil {
		if errors.Is(err, provider.ErrVulnerabilitiesUnavailable) {
			g.mu.Lock()
			g.off = true
			g.mu.Unlock()
			lg.Info("graph: security domain unavailable, vuln ingest off", "err", err)
			return nil, false
		}
		if ctx.Err() == nil {
			lg.Warn("graph: vulnerabilities fetch failed, repo skipped", "err", err)
		}
		return nil, false
	}
	points := make([]*qdrant.PointStruct, 0, len(findings))
	for _, f := range findings {
		points = append(points, graph.VulnPoint(meta, f.ID, f.Severity, f.Scanner, f.State, f.LocationFile))
	}
	if len(points) > 0 {
		lg.Info("graph: vuln findings fetched", "findings", len(points))
	}
	return points, true
}

// runnerGate latches the availability of the runner domain for one graph lane
// (US4/T037), mirroring vulnGate: the first call probes; a typed
// provider.ErrDomainUnavailable (403/404, missing role/scope) turns the domain
// off for the process and is logged once; transient provider errors are logged
// and left to the next repo/pass. ok reports whether the listing of this repo
// completed — only then may Sweep generation-sweep runner nodes; a latched-off
// or failed domain keeps its stale points instead of deleting them blindly.
type runnerGate struct {
	mu  sync.Mutex
	off bool
}

// ingest reads the runners of one repository and returns their node points.
// It returns nil,false when the provider has no runner support, the domain is
// off or the call failed — never an error.
func (g *runnerGate) ingest(ctx context.Context, deps RepoDeps, repo provider.Repo, meta graph.Meta, host string, lg *slog.Logger) ([]*qdrant.PointStruct, bool) {
	rp, ok := deps.VCS.(provider.RunnersProvider)
	if !ok {
		return nil, false
	}
	g.mu.Lock()
	off := g.off
	g.mu.Unlock()
	if off {
		return nil, false
	}
	runners, err := rp.ListRunners(ctx, repo)
	if err != nil {
		if errors.Is(err, provider.ErrDomainUnavailable) {
			g.mu.Lock()
			g.off = true
			g.mu.Unlock()
			lg.Info("graph: runner domain unavailable, runner ingest off", "err", err)
			return nil, false
		}
		if ctx.Err() == nil {
			lg.Warn("graph: runners fetch failed, repo skipped", "err", err)
		}
		return nil, false
	}
	points := make([]*qdrant.PointStruct, 0, len(runners))
	for _, r := range runners {
		points = append(points, graph.RunnerPoint(meta, host, r))
	}
	if len(points) > 0 {
		lg.Info("graph: runners fetched", "runners", len(points))
	}
	return points, true
}

// securityPoints builds the schema-v3 security sub-family points of one
// repository (T018/US1): the findings listing carries its own vulnerability,
// finding, occurrence, security_scan, vulnerability_scanner and identifier
// nodes plus the relations between them (in_project, has_finding,
// occurrence_of, detected_by, has_identifier, in_pipeline). The domain is
// runtime-gated: a provider without the SecurityVulnReader seam or a typed
// unavailable (403/404, missing Ultimate tier or Security Dashboard, missing
// scope) is skipped — logged, never a pass failure and never a GAP (FR-024).
// ok reports whether the listing completed: only then may Sweep
// generation-sweep the security kinds, so an unavailable domain keeps its
// stale points. The sub-objects one listing repeats across findings
// (vulnerability, scan, scanner, identifier) are deduped by their
// deterministic point ID; deterministic IDs make the re-upsert each pass
// idempotent.
func securityPoints(ctx context.Context, deps RepoDeps, repo provider.Repo, meta graph.Meta, host string, pipelineKeys map[string]string, lg *slog.Logger) ([]*qdrant.PointStruct, bool) {
	reader, ok := deps.VCS.(provider.SecurityVulnReader)
	if !ok {
		return nil, false
	}
	findings, err := reader.ListSecurityFindings(ctx, repo)
	if err != nil {
		if ctx.Err() != nil {
			return nil, false
		}
		if errors.Is(err, provider.ErrVulnerabilitiesUnavailable) || errors.Is(err, provider.ErrDomainUnavailable) {
			lg.Info("graph: security domain unavailable, skipped", "err", err)
			return nil, false
		}
		lg.Warn("graph: security findings fetch failed, skipped", "err", err)
		return nil, false
	}
	projectKey := graph.RefProject(meta.Repo)
	points := make([]*qdrant.PointStruct, 0, len(findings)*8)
	seen := make(map[string]bool, len(findings)*8)
	// add appends a point once: the deterministic IDs dedupe the sub-objects
	// and relations a listing repeats across findings (vulnerability, scan,
	// scanner, identifier; their in_project/has_identifier edges).
	add := func(p *qdrant.PointStruct) {
		id := p.GetId().GetUuid()
		if seen[id] {
			return
		}
		seen[id] = true
		points = append(points, p)
	}
	for _, f := range findings {
		if ctx.Err() != nil {
			break
		}
		vulnKey := securityVulnKey(meta.Repo, f)
		findingKey := graph.RefFinding(meta.Repo, f.UUID)
		occurrenceKey := graph.RefVulnerabilityOccurrence(meta.Repo, f.UUID)
		scanKey := graph.RefSecurityScan(meta.Repo, f.ScanID)
		scannerKey := graph.RefVulnerabilityScanner(host, f.ScannerID)

		add(graph.FindingPoint(meta, f))
		add(graph.VulnerabilityOccurrencePoint(meta, f))
		add(graph.VulnerabilityPoint(meta, f))
		add(graph.SecurityScanPoint(meta, f))
		add(graph.VulnerabilityScannerPoint(meta, host, f))
		add(graph.InProjectEdge(meta, findingKey, "finding", projectKey))
		add(graph.InProjectEdge(meta, occurrenceKey, "vulnerability_occurrence", projectKey))
		add(graph.InProjectEdge(meta, vulnKey, "vulnerability", projectKey))
		add(graph.InProjectEdge(meta, scanKey, "security_scan", projectKey))
		add(graph.InProjectEdge(meta, scannerKey, "vulnerability_scanner", projectKey))
		add(graph.HasFindingEdge(meta, scanKey, "security_scan", findingKey, "finding"))
		add(graph.HasFindingEdge(meta, vulnKey, "vulnerability", occurrenceKey, "vulnerability_occurrence"))
		add(graph.OccurrenceOfEdge(meta, occurrenceKey, vulnKey))
		add(graph.DetectedByEdge(meta, findingKey, "finding", scannerKey))
		add(graph.DetectedByEdge(meta, occurrenceKey, "vulnerability_occurrence", scannerKey))
		for _, ident := range f.Identifiers {
			identKey := graph.RefVulnerabilityIdentifier(host, ident.Name)
			add(graph.VulnerabilityIdentifierPoint(meta, host, ident))
			add(graph.InProjectEdge(meta, identKey, "vulnerability_identifier", projectKey))
			add(graph.HasIdentifierEdge(meta, findingKey, "finding", identKey))
			add(graph.HasIdentifierEdge(meta, occurrenceKey, "vulnerability_occurrence", identKey))
		}
		// in_pipeline: only when the pipeline's ci point carries a canonical
		// node_key; today's ci points carry none, so the edge is skipped
		// rather than left dangling.
		if f.ScanPipelineID != "" {
			if ciKey := pipelineKeys[f.ScanPipelineID]; ciKey != "" {
				add(graph.InPipelineEdge(meta, scanKey, ciKey))
			}
		}
	}
	if len(points) > 0 {
		lg.Info("graph: security findings fetched", "findings", len(findings), "points", len(points))
	}
	return points, true
}

// securityVulnKey mirrors security.go's vulnerabilityKey: a GitLab finding
// carries vulnerability_id; a provider without one falls back to the finding
// id, so the vulnerability node, the occurrence's vulnerability_id link and
// the has_finding edge agree.
func securityVulnKey(repo string, f provider.VulnerabilityFinding) string {
	id := f.VulnerabilityID
	if id == "" {
		id = f.ID
	}
	return graph.RefVulnerability(repo, id)
}

// ciPipelineKeys indexes the canonical ci node_keys of one pass by pipeline
// id: in_pipeline may only join a scan whose pipeline has a ci point carrying
// a node_key. Today's ci points carry none (ci.go), so the map is empty and
// the edge is skipped — never a dangling endpoint.
func ciPipelineKeys(ciPts []*qdrant.PointStruct) map[string]string {
	keys := make(map[string]string, len(ciPts))
	for _, p := range ciPts {
		m := p.GetPayload()
		pid := m["pipeline_id"].GetStringValue()
		key := m["node_key"].GetStringValue()
		if pid != "" && key != "" {
			keys[pid] = key
		}
	}
	return keys
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

// structurePoints builds the tree/branch step of one repository (US3/T034):
// one branch node per branch of the provider's listing, the project→branch
// contains edge for every branch, and — for the default branch — the
// branch→root-directory contains plus the on_branch edges of every scanned
// file and directory. The pass-branch on_branch edges of the scanned tree are
// already written by the code-graph step (graph.WriteGraph); the deterministic
// edge IDs keep the overlap idempotent. Best-effort by design (FR-024): a
// provider without the TreeReader seam or a failed listing is logged and the
// pass continues; the returned flag reports whether the listing completed —
// only then may Sweep generation-sweep branch nodes (targets.Structure).
// Deterministic IDs make the re-upsert each pass idempotent.
func structurePoints(ctx context.Context, deps RepoDeps, repo provider.Repo, meta graph.Meta, scanned []graph.FileGraph, lg *slog.Logger) ([]*qdrant.PointStruct, bool) {
	tr, ok := deps.VCS.(provider.TreeReader)
	if !ok {
		return nil, false
	}
	branches, err := tr.ListBranches(ctx, repo)
	if err != nil {
		orbitFail(ctx, lg, "branches", err)
		return nil, false
	}
	projectKey := graph.RefProject(meta.Repo)
	rootDir := graph.RefDirectory(meta.Repo, "")
	paths, dirs := treePaths(scanned)
	var points []*qdrant.PointStruct
	for _, b := range branches {
		branchKey := graph.RefBranch(meta.Repo, b.Name)
		points = append(points, graph.BranchPoint(meta, b))
		points = append(points, graph.ContainsEdge(meta, projectKey, "project", branchKey, "branch"))
		// The default branch carries the tree; an empty scan has no
		// file/directory nodes, so its edges are skipped rather than left
		// dangling.
		if !b.Default || len(paths) == 0 {
			continue
		}
		points = append(points, graph.ContainsEdge(meta, branchKey, "branch", rootDir, "directory"))
		for _, d := range dirs {
			points = append(points, graph.OnBranchEdge(meta, graph.RefDirectory(meta.Repo, d), "directory", branchKey))
		}
		for _, p := range paths {
			points = append(points, graph.OnBranchEdge(meta, graph.RefFile(meta.Repo, p), "file", branchKey))
		}
	}
	if len(points) > 0 {
		lg.Info("graph: branch nodes fetched", "branches", len(branches), "points", len(points))
	}
	return points, true
}

// treePaths derives the sorted file and directory paths of one scan the
// structure step attaches to the default branch: the union of the FileGraph
// paths and the definition file_paths, with the repository root spelled ""
// (mirrors graph.StructurePoints, which owns the equivalent derivation for the
// file/directory points themselves).
func treePaths(scanned []graph.FileGraph) (files, dirs []string) {
	fileSet := make(map[string]bool)
	for _, f := range scanned {
		if f.Path != "" {
			fileSet[f.Path] = true
		}
		for _, d := range f.Defs {
			if d.FilePath != "" {
				fileSet[d.FilePath] = true
			}
		}
	}
	dirSet := make(map[string]bool)
	for p := range fileSet {
		for d := path.Dir(p); ; d = path.Dir(d) {
			if d == "." {
				d = ""
			}
			if dirSet[d] {
				break
			}
			dirSet[d] = true
			if d == "" {
				break
			}
		}
	}
	files = make([]string, 0, len(fileSet))
	for p := range fileSet {
		files = append(files, p)
	}
	dirs = make([]string, 0, len(dirSet))
	for d := range dirSet {
		dirs = append(dirs, d)
	}
	sort.Strings(files)
	sort.Strings(dirs)
	return files, dirs
}

// ownershipPoints builds the ownership step of one repository (US4/T039): one
// group node per group of the provider's listing, the parent→child contains
// edge where the namespace hierarchy names a parent, and per group the project
// nodes with their group→project contains edge. Projects are listed per group
// so each carries its namespace context. Best-effort by design (FR-024): a
// provider without the GroupReader seam or a failed listing is logged and the
// pass continues; the returned flag reports whether every listing of the step
// completed — only then may Sweep generation-sweep the group/project kinds
// (targets.Ownership), so a withheld listing keeps its stale points.
// Deterministic IDs make the re-upsert each pass idempotent.
func ownershipPoints(ctx context.Context, deps RepoDeps, meta graph.Meta, host string, lg *slog.Logger) ([]*qdrant.PointStruct, bool) {
	gr, ok := deps.VCS.(provider.GroupReader)
	if !ok {
		return nil, false
	}
	groups, err := gr.ListGroups(ctx)
	if err != nil {
		orbitFail(ctx, lg, "groups", err)
		return nil, false
	}
	complete := true
	var points []*qdrant.PointStruct
	for _, g := range groups {
		if ctx.Err() != nil {
			return points, false
		}
		gp := graph.GroupPoint(meta, g)
		groupKey := gp.GetPayload()["node_key"].GetStringValue()
		points = append(points, gp)
		if parentKey := gp.GetPayload()["parent_key"].GetStringValue(); parentKey != "" {
			points = append(points, graph.GroupContainsGroupEdge(meta, parentKey, groupKey))
		}
		projects, err := gr.ListGroupProjects(ctx, g.FullPath)
		if err != nil {
			complete = false
			orbitFail(ctx, lg, "group projects", err)
			continue
		}
		for _, p := range projects {
			points = append(points, graph.ProjectPoint(meta, host, p))
			points = append(points, graph.GroupContainsProjectEdge(meta, groupKey, graph.RefProject(p.FullPath)))
		}
	}
	if complete && len(points) > 0 {
		lg.Info("graph: ownership nodes fetched", "groups", len(groups), "points", len(points))
	}
	return points, complete
}
