package worker

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gorizond/vcs-rag/internal/config"
	"github.com/gorizond/vcs-rag/internal/embed"
	"github.com/gorizond/vcs-rag/internal/gitengine"
	"github.com/gorizond/vcs-rag/internal/log"
	"github.com/gorizond/vcs-rag/internal/provider"
	"github.com/gorizond/vcs-rag/internal/store"
)

// runMetadata is the metadata lane (T054): on the WORKER_META_INTERVAL tick
// (default WORKER_INTERVAL) it processes registry repositories through the
// pool; errors do not break the loop (FR-014).
func runMetadata(ctx context.Context, cfg *config.Config, deps RepoDeps, snap *snapshot) error {
	deps.Logger = log.WithPhase(deps.Logger, "metadata")
	metadataAll(ctx, cfg, deps, snap)

	ticker := time.NewTicker(cfg.Worker.MetaInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			metadataAll(ctx, cfg, deps, snap)
		}
	}
}

// metadataAll processes metadata of all known repositories through the pool;
// failures are isolated (FR-014), a summary is written after the pass (FR-020).
func metadataAll(ctx context.Context, cfg *config.Config, deps RepoDeps, snap *snapshot) {
	repos := snap.get()
	if len(repos) == 0 {
		return
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
				return ProcessMetadataOnce(ctx, deps, repo)
			}))
		}(repo)
	}
	wg.Wait()
	sum.log(deps.Logger)
}

// metaStore is the store surface of the metadata lane: a narrow interface so
// unit tests can count embed/upsert calls without Qdrant (the mcp.Searcher pattern).
type metaStore interface {
	Upsert(ctx context.Context, points []store.Point) error
	ScrollCIFailures(ctx context.Context, repo string, limit uint32) ([]store.ScoredPoint, error)
	DeleteCIFailure(ctx context.Context, repo, pipelineID, jobID string) error
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
	if err := pruneCIFailures(ctx, st, repo, current, deps.stats); err != nil {
		return err
	}
	pruneSeenCI(seen, current)

	// the sidecar is shared with the mirror/index lanes: write under the mutex.
	if err := updateState(statePath, func(s *MirrorState) { s.Meta = seen }); err != nil {
		return fmt.Errorf("state: %w", err)
	}

	skipped := (len(cands) - len(pending)) + (ciFailed - len(ciCands))
	lg.Info("metadata updated", "issues", len(issues), "prs", len(prs),
		"ci_failures", ciFailed, "skipped", skipped)
	return nil
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
