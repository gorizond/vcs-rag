package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
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
// never sweeps. The orbit phases (US2/US3) run after the code graph: the
// change-request list feeds both the mr_file diffs and the change-request
// notes, then the planning nodes (work items, milestones, labels) and the
// users/notes. The runner nodes (US4) come behind their lane-wide gate last.
// Every orbit domain is best-effort: a failed or unavailable listing withholds
// only that domain's sweep (SweepTargets) and never the pass. CPU-only: no
// embeddings, no commit budget (R13). Lock-free by design: the lane and the
// refresh composition hold the per-repo lock.
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
	// US3/T031 + US2/T023: the orbit nodes — work items, milestones, labels,
	// then the users and their notes/comments. Each domain is best-effort and
	// idempotent (deterministic IDs); the targets tell Sweep which kinds saw a
	// complete listing and may be generation-swept.
	host := vcsHost(cfg, deps.VCSType)
	planning, items, targets := workItemPoints(ctx, deps, repo, meta, host, lg)
	people, userSweep := userNotePoints(ctx, deps, repo, meta, host, items, crs, targets.WorkItems, lg)
	targets.Users = userSweep
	if orbitPts := append(planning, people...); len(orbitPts) > 0 {
		if err := gs.Upsert(ctx, orbitPts); err != nil {
			return written, fmt.Errorf("orbit write: %w", err)
		}
		written += len(orbitPts)
	}
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
