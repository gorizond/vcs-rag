package worker

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qdrant/go-client/qdrant"

	"github.com/gorizond/vcs-rag/internal/config"
	"github.com/gorizond/vcs-rag/internal/embed"
	"github.com/gorizond/vcs-rag/internal/gitengine"
	"github.com/gorizond/vcs-rag/internal/log"
	"github.com/gorizond/vcs-rag/internal/provider"
	"github.com/gorizond/vcs-rag/internal/store"
)

// runMetadata is the metadata lane (T054): an adaptive paced loop (T129) —
// WORKER_META_INTERVAL (default WORKER_INTERVAL) is the ceiling, not a tick.
// Errors do not break the loop (FR-014).
func runMetadata(ctx context.Context, cfg *config.Config, deps RepoDeps, snap *snapshot) error {
	deps.Logger = log.WithPhase(deps.Logger, "metadata")
	p := newPacer(cfg.Worker.MetaInterval)
	// T129: a random start U[0, 2s] avoids a boot herd across lanes.
	if err := p.sleep(ctx, p.startDelay()); err != nil {
		return nil
	}
	lastRepos := -1
	for {
		sum := metadataAll(ctx, cfg, deps, snap)
		busy := sum != nil && (sum.updated.Load() > 0 || sum.failed.Load() > 0)
		rl, retry := false, time.Duration(0)
		if sum != nil {
			rl, retry = sum.rateLimited()
		}
		if n := len(snap.get()); n > 0 && lastRepos >= 0 && n != lastRepos {
			busy = true
		}
		lastRepos = len(snap.get())
		delay := p.next(busy, rl, retry)
		if sum != nil {
			sum.withPace(delay, p.reason)
			if l, ok := deps.Embed.(*limiter); ok {
				sum.withEmbed(l.stats())
			}
			sum.log(deps.Logger)
		}
		if err := p.sleep(ctx, delay); err != nil {
			return nil
		}
	}
}

// metadataAll processes metadata of all known repositories through the pool;
// failures are isolated (FR-014). It returns the pass summary (nil — nothing to
// do) for the pacer and the pass log (T129).
func metadataAll(ctx context.Context, cfg *config.Config, deps RepoDeps, snap *snapshot) *summary {
	repos := snap.get()
	if len(repos) == 0 {
		return nil
	}
	sum := newSummary("metadata")
	deps.stats = sum
	p := newPool(cfg.Worker.Concurrency)
	var wg sync.WaitGroup
	for _, repo := range repos {
		wg.Add(1)
		go func(repo provider.Repo) {
			defer wg.Done()
			if err := p.Acquire(ctx); err != nil {
				return
			}
			defer p.Release()
			sum.countRepo(processSafely(ctx, deps.Logger, repo, "metadata", func() error {
				// T110: the per-repo lock is taken at the call site only.
				return withRepoLock(repoSlug(repo.Namespace+"/"+repo.Path), func() error {
					return ProcessMetadataOnce(ctx, deps, repo)
				})
			}))
		}(repo)
	}
	wg.Wait()
	return sum
}

// metaStore is the store surface of the metadata lane: a narrow interface so
// unit tests can count embed/upsert calls without Qdrant (the mcp.Searcher pattern).
type metaStore interface {
	Upsert(ctx context.Context, points []store.Point) error
	ScrollCIFailures(ctx context.Context, repo string, limit uint32) ([]store.ScoredPoint, error)
	DeleteCIFailure(ctx context.Context, repo, pipelineID, jobID string) error
	DeleteByIDs(ctx context.Context, ids []string) error // T140 entity reconcile
	// SetPayload — the F3 payload-only update of already-embedded points (T029).
	SetPayload(ctx context.Context, ids []string, payload map[string]*qdrant.Value) error
}

// Entity-reconcile parameters (T140).
const (
	metaMissThreshold = 2  // consecutive absent passes before a point is deleted
	metaDeletePerRepo = 32 // candidates considered per repository per pass
)

// entity is one vanished issue/MR key ready for deletion (T140).
type entity struct {
	key  string
	kind string // "issue" | "mr"
	id   int64
}

// entityKey parses an entity key; CI keys ("ci:...") and foreign keys are not
// entities (false).
func entityKey(key string) (kind string, id int64, ok bool) {
	kind, rest, found := strings.Cut(key, ":")
	if !found || (kind != "issue" && kind != "mr") {
		return "", 0, false
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil {
		return "", 0, false
	}
	return kind, id, true
}

// metaCandidate is a metadata point with its state key and source timestamp.
type metaCandidate struct {
	key       string    // "issue:<id>", "mr:<id>", "ci:<pipeline>:<job>"
	updatedAt time.Time // issue/MR updated_at; zero for CI (indexed once)
	item      metaItem
}

// pendingMeta selects candidates missing from the state or changed since the
// last pass (T080): only changed/new items are re-embedded and upserted.
func pendingMeta(cands []metaCandidate, seen map[string]int64) []metaCandidate {
	var out []metaCandidate
	for _, c := range cands {
		prev, ok := seen[c.key]
		if ok && (c.updatedAt.IsZero() || prev == c.updatedAt.Unix()) {
			continue // CI job — indexed once (rerun gets new ids); issue/MR — unchanged
		}
		out = append(out, c)
	}
	return out
}

// markMeta records successfully written candidates in the state.
func markMeta(seen map[string]int64, cands []metaCandidate) {
	for _, c := range cands {
		if c.updatedAt.IsZero() {
			seen[c.key] = 1
			continue
		}
		seen[c.key] = c.updatedAt.Unix()
	}
}

// pruneSeenCI drops state keys of ci points gone from the current window.
func pruneSeenCI(seen map[string]int64, current map[[2]string]bool) {
	for key := range seen {
		rest, ok := strings.CutPrefix(key, "ci:")
		if !ok {
			continue
		}
		pipeline, job, ok := strings.Cut(rest, ":")
		if ok && !current[[2]string{pipeline, job}] {
			delete(seen, key)
		}
	}
}

// itemsOf extracts the points of the candidates.
func itemsOf(cands []metaCandidate) []metaItem {
	items := make([]metaItem, len(cands))
	for i, c := range cands {
		items[i] = c.item
	}
	return items
}

// checkpointMeta merges freshly written keys into Meta (T090): a crash between
// the issues/MR and CI phases keeps the progress of the written items — the
// next pass skips them. Only Meta is touched; the fresh state's branches and
// last_fetch_at (other lanes) are preserved.
func checkpointMeta(statePath string, seen map[string]int64) error {
	return updateState(statePath, func(st *MirrorState) {
		for key, at := range seen {
			st.Meta[key] = at
		}
	})
}

// ProcessMetadataOnce is one metadata pass of a repository: issues + MR/PR
// (upsert by deterministic IDs updates state/text), then CI failures
// (tail ≤ ciTailLines lines) and prune of ci points gone from the current window.
// Incremental (T080): a point is re-embedded only when it is missing from the
// mirror state or its updated_at changed; a CI job is indexed once; the first
// pass (empty state) is full. Extracted for the e2e test (T055).
func ProcessMetadataOnce(ctx context.Context, deps RepoDeps, repo provider.Repo) error {
	return processMetadataOnce(ctx, deps, repo, deps.Store)
}

// processMetadataOnce is ProcessMetadataOnce over the narrow metaStore interface.
func processMetadataOnce(ctx context.Context, deps RepoDeps, repo provider.Repo, st metaStore) error {
	lg := log.WithRepo(deps.Logger, repo.Namespace+"/"+repo.Path)
	statePath := gitengine.StatePath(deps.Engine.MirrorPath(deps.VCSType, repo))
	mirror, err := LoadState(statePath)
	if err != nil {
		return fmt.Errorf("state: %w", err)
	}
	seen := mirror.Meta
	miss := mirror.MetaMiss

	// --- issues + MR/PR: embed only new/changed ---
	issues, err := deps.VCS.ListIssues(ctx, repo)
	if err != nil {
		return fmt.Errorf("issues: %w", err)
	}
	prs, err := deps.VCS.ListChangeRequests(ctx, repo)
	if err != nil {
		return fmt.Errorf("MR/PR: %w", err)
	}
	cands := make([]metaCandidate, 0, len(issues)+len(prs))
	for _, iss := range issues {
		cands = append(cands, metaCandidate{
			key:       fmt.Sprintf("issue:%d", iss.ID),
			updatedAt: iss.UpdatedAt,
			item:      mapIssue(deps.VCSType, repo.Namespace, repo.Path, iss),
		})
	}
	for _, cr := range prs {
		cands = append(cands, metaCandidate{
			key:       fmt.Sprintf("mr:%d", cr.ID),
			updatedAt: cr.UpdatedAt,
			item:      mapChangeRequest(deps.VCSType, repo.Namespace, repo.Path, cr),
		})
	}
	pending := pendingMeta(cands, seen)
	if err := upsertItems(ctx, deps.Embed, st, itemsOf(pending)); err != nil {
		return err
	}
	markMeta(seen, pending)
	// T090: checkpoint after the issues/MR phase — a failure later in the pass
	// keeps this progress (the retry skips the written items).
	if len(pending) > 0 {
		if err := checkpointMeta(statePath, seen); err != nil {
			return fmt.Errorf("state: %w", err)
		}
	}
	// T029/F3: points that were not re-embedded get the additive payload fields
	// via set_payload — no vectors, no LM spend. Runs after the checkpoint so a
	// failed payload write can never cause a re-embed of the batch next pass.
	refreshMetaPayload(ctx, st, cands, pending, lg)

	// --- CI: failed jobs of the current window; a job is indexed once ---
	pipelines, err := deps.VCS.ListPipelines(ctx, repo)
	if err != nil {
		return fmt.Errorf("pipelines: %w", err)
	}
	var ciCands []metaCandidate
	ciFailed := 0
	current := make(map[[2]string]bool)
	for _, p := range pipelines {
		for _, j := range p.Jobs {
			if !jobFailed(j) {
				continue
			}
			ciFailed++
			current[[2]string{p.ID, j.ID}] = true
			key := "ci:" + p.ID + ":" + j.ID
			if _, ok := seen[key]; ok {
				continue // already indexed (a rerun creates new ids)
			}
			tail, err := deps.VCS.PipelineLogTail(ctx, repo, p, j, ciTailLines)
			if err != nil {
				// the log is unavailable — the point is still useful without the tail;
				// the error does not break the pass (FR-014).
				lg.Error("job log tail not fetched", "pipeline", p.ID, "job", j.ID, "err", err)
				tail = ""
			}
			ciCands = append(ciCands, metaCandidate{
				key:  key,
				item: mapCIFailure(deps.VCSType, repo.Namespace, repo.Path, p, j, tail),
			})
		}
	}
	if err := upsertItems(ctx, deps.Embed, st, itemsOf(ciCands)); err != nil {
		return err
	}
	markMeta(seen, ciCands)
	// T090: checkpoint after the CI phase (the prune below may still fail).
	if len(ciCands) > 0 {
		if err := checkpointMeta(statePath, seen); err != nil {
			return fmt.Errorf("state: %w", err)
		}
	}
	if err := pruneCIFailures(ctx, st, repo, current, deps.stats); err != nil {
		return err
	}
	pruneSeenCI(seen, current)

	// --- entity reconcile (T140): reuse the full issue/MR listings above ---
	reconcileEntityMeta(ctx, deps, st, repo, seen, miss, cands, deps.stats)

	// the sidecar is shared with the mirror/index lanes: the terminal write
	// replaces Meta/MetaMiss (prune removals included) and stamps the
	// successful metadata pass (T135) under the mutex.
	if err := updateState(statePath, func(s *MirrorState) {
		s.Meta = seen
		s.MetaMiss = miss
		s.MetaSyncedAt = time.Now().UTC()
	}); err != nil {
		return fmt.Errorf("state: %w", err)
	}

	skipped := (len(cands) - len(pending)) + (ciFailed - len(ciCands))
	if deps.stats != nil {
		deps.stats.addSkipped(skipped)                     // T091
		deps.stats.addUpdated(len(pending) + len(ciCands)) // T129 busy signal
	}
	lg.Info("metadata updated", "issues", len(issues), "prs", len(prs),
		"ci_failures", ciFailed, "skipped", skipped)
	return nil
}

// reconcileEntityMeta is the entity-GC phase (T140) of one metadata pass: it
// reuses the full issue/MR listings already fetched by the pass — no extra API
// calls. A key absent from the listing twice in a row (MetaMiss) loses its
// point and the Meta key; a returned key resets its counter. CI keys are owned
// by the existing prune (pruneCIFailures/pruneSeenCI) and are never touched.
func reconcileEntityMeta(ctx context.Context, deps RepoDeps, st metaStore, repo provider.Repo,
	seen map[string]int64, miss map[string]int, cands []metaCandidate, sum *summary) {
	if sum == nil {
		sum = newSummary("metadata")
	}
	current := make(map[string]bool, len(cands))
	for _, c := range cands {
		current[c.key] = true
	}
	var toDelete []entity
	for key := range seen {
		kind, id, ok := entityKey(key)
		if !ok {
			continue
		}
		if current[key] {
			delete(miss, key) // the entity returned — reset the counter
			continue
		}
		miss[key]++
		if miss[key] >= metaMissThreshold {
			toDelete = append(toDelete, entity{key: key, kind: kind, id: id})
		}
	}
	sort.Slice(toDelete, func(i, j int) bool { return toDelete[i].key < toDelete[j].key })
	sum.reconciled.Add(int64(len(toDelete))) // candidates found (before the caps)
	if len(toDelete) > metaDeletePerRepo {
		sum.metaSuppressedCap.Add(int64(len(toDelete) - metaDeletePerRepo))
		toDelete = toDelete[:metaDeletePerRepo]
	}
	if len(toDelete) == 0 {
		return
	}
	full := repo.Namespace + "/" + repo.Path
	// the dry-run kill-switch (default) also holds for the entity GC.
	if deps.Cfg == nil || deps.Cfg.Worker.GCDryRun {
		issues, mrs := 0, 0
		for _, e := range toDelete {
			if e.kind == "issue" {
				issues++
			} else {
				mrs++
			}
		}
		deps.Logger.Info("reconcile would-delete", "repo", full, "issues", issues, "mrs", mrs)
		return
	}
	// real mode: the pass budget is shared by all repositories; keys are
	// dropped only after a successful batch delete (a failed one stays).
	byKind := map[string][]entity{}
	for _, e := range toDelete {
		if !sum.reserveMetaDelete() {
			sum.metaSuppressedCap.Add(1)
			continue
		}
		byKind[e.kind] = append(byKind[e.kind], e)
	}
	for kind, batch := range byKind {
		ids := make([]string, 0, len(batch))
		for _, e := range batch {
			if kind == "issue" {
				ids = append(ids, store.IssueID(deps.VCSType, repo.Path, e.id))
			} else {
				ids = append(ids, store.PullRequestID(deps.VCSType, repo.Path, e.id))
			}
		}
		if err := st.DeleteByIDs(ctx, ids); err != nil {
			sum.failed.Add(1)
			deps.Logger.Warn("reconcile delete failed", "repo", full, "kind", kind, "count", len(ids), "err", err)
			continue
		}
		for _, e := range batch {
			delete(seen, e.key)
			delete(miss, e.key)
		}
		sum.goneKeys.Add(int64(len(batch)))
		sum.deletedEntities.Add(int64(len(batch)))
	}
}

// upsertItems embeds the texts (batches inside the provider) and writes points.
func upsertItems(ctx context.Context, emb embed.Provider, st metaStore, items []metaItem) error {
	if len(items) == 0 {
		return nil
	}
	texts := make([]string, len(items))
	for i, it := range items {
		texts[i] = it.Text
	}
	vecs, err := emb.Embed(ctx, texts)
	if err != nil {
		return fmt.Errorf("metadata embeddings: %w", err)
	}
	points := make([]store.Point, len(items))
	for i, it := range items {
		points[i] = store.Point{ID: it.ID, Vector: vecs[i], Payload: it.Payload}
	}
	return st.Upsert(ctx, points)
}

// refreshMetaPayload writes the F3 fields (T029) of the candidates that were
// NOT re-embedded this pass — the pending ones already carry them in the
// upsert payload. Payload-only by design: a re-upsert would re-embed the
// batch and break the LM budget. Failures are logged, not fatal: the next
// pass retries, and the state checkpoint already prevents a re-embed.
// ponytail: one SetPayload RPC per unchanged entity per pass; batch via
// UpdateBatchPoints if the write volume ever matters.
func refreshMetaPayload(ctx context.Context, st metaStore, cands, pending []metaCandidate, lg *slog.Logger) {
	if len(pending) == len(cands) {
		return
	}
	reembedded := make(map[string]bool, len(pending))
	for _, c := range pending {
		reembedded[c.key] = true
	}
	for _, c := range cands {
		if reembedded[c.key] || len(c.item.MetaPayload) == 0 {
			continue
		}
		if err := st.SetPayload(ctx, []string{c.item.ID}, store.PayloadValues(c.item.MetaPayload)); err != nil {
			lg.Warn("F3 set_payload failed", "id", c.item.ID, "err", err)
			return // likely a backend outage — do not hammer the rest
		}
	}
}

// pruneCIFailures deletes repository ci points missing from the current
// (pipeline_id, job_id) set — e.g. after a job was rerun to success.
func pruneCIFailures(ctx context.Context, st metaStore, repo provider.Repo, current map[[2]string]bool, stats *summary) error {
	existing, err := st.ScrollCIFailures(ctx, repo.Path, 1000)
	if err != nil {
		return err
	}
	pruned := 0
	for _, p := range existing {
		key := [2]string{payloadString(p.Payload, "pipeline_id"), payloadString(p.Payload, "job_id")}
		if current[key] {
			continue
		}
		if err := st.DeleteCIFailure(ctx, repo.Path, key[0], key[1]); err != nil {
			return err
		}
		pruned++
	}
	if stats != nil {
		stats.addPruned(pruned)
	}
	return nil
}

// payloadString reads a string payload field.
func payloadString(p map[string]any, key string) string {
	s, _ := p[key].(string)
	return s
}
