package worker

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/gorizond/vcs-rag/internal/config"
	"github.com/gorizond/vcs-rag/internal/log"
	"github.com/gorizond/vcs-rag/internal/provider"
	"github.com/gorizond/vcs-rag/internal/store"
)

// runMetadata is the metadata lane (T054): on the WORKER_INTERVAL tick it
// processes registry repositories through the pool; errors do not break the loop (FR-014).
func runMetadata(ctx context.Context, cfg *config.Config, deps RepoDeps, snap *snapshot) error {
	deps.Logger = log.WithPhase(deps.Logger, "metadata")
	metadataAll(ctx, cfg, deps, snap)

	ticker := time.NewTicker(cfg.Worker.Interval)
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

// ProcessMetadataOnce is one metadata pass of a repository: issues + MR/PR
// (upsert by deterministic IDs updates state/text), then CI failures
// (tail ≤ ciTailLines lines) and prune of ci points gone from the current window.
// Extracted for the e2e test (T055).
func ProcessMetadataOnce(ctx context.Context, deps RepoDeps, repo provider.Repo) error {
	lg := log.WithRepo(deps.Logger, repo.Namespace+"/"+repo.Path)

	// --- issues + MR/PR ---
	issues, err := deps.VCS.ListIssues(ctx, repo)
	if err != nil {
		return fmt.Errorf("issues: %w", err)
	}
	prs, err := deps.VCS.ListChangeRequests(ctx, repo)
	if err != nil {
		return fmt.Errorf("MR/PR: %w", err)
	}
	items := make([]metaItem, 0, len(issues)+len(prs))
	for _, iss := range issues {
		items = append(items, mapIssue(deps.VCSType, repo.Namespace, repo.Path, iss))
	}
	for _, cr := range prs {
		items = append(items, mapChangeRequest(deps.VCSType, repo.Namespace, repo.Path, cr))
	}
	if err := upsertItems(ctx, deps, items); err != nil {
		return err
	}

	// --- CI: failed jobs of the current window ---
	pipelines, err := deps.VCS.ListPipelines(ctx, repo)
	if err != nil {
		return fmt.Errorf("pipelines: %w", err)
	}
	var ciItems []metaItem
	current := make(map[[2]string]bool)
	for _, p := range pipelines {
		for _, j := range p.Jobs {
			if !jobFailed(j) {
				continue
			}
			tail, err := deps.VCS.PipelineLogTail(ctx, repo, p, j, ciTailLines)
			if err != nil {
				// the log is unavailable — the point is still useful without the tail;
				// the error does not break the pass (FR-014).
				lg.Error("job log tail not fetched", "pipeline", p.ID, "job", j.ID, "err", err)
				tail = ""
			}
			ciItems = append(ciItems, mapCIFailure(deps.VCSType, repo.Namespace, repo.Path, p, j, tail))
			current[[2]string{p.ID, j.ID}] = true
		}
	}
	if err := upsertItems(ctx, deps, ciItems); err != nil {
		return err
	}
	if err := pruneCIFailures(ctx, deps, repo, current); err != nil {
		return err
	}

	lg.Info("metadata updated", "issues", len(issues), "prs", len(prs), "ci_failures", len(ciItems))
	return nil
}

// upsertItems embeds the texts (batches inside the provider) and writes points.
func upsertItems(ctx context.Context, deps RepoDeps, items []metaItem) error {
	if len(items) == 0 {
		return nil
	}
	texts := make([]string, len(items))
	for i, it := range items {
		texts[i] = it.Text
	}
	vecs, err := deps.Embed.Embed(ctx, texts)
	if err != nil {
		return fmt.Errorf("metadata embeddings: %w", err)
	}
	points := make([]store.Point, len(items))
	for i, it := range items {
		points[i] = store.Point{ID: it.ID, Vector: vecs[i], Payload: it.Payload}
	}
	return deps.Store.Upsert(ctx, points)
}

// pruneCIFailures deletes repository ci points missing from the current
// (pipeline_id, job_id) set — e.g. after a job was rerun to success.
func pruneCIFailures(ctx context.Context, deps RepoDeps, repo provider.Repo, current map[[2]string]bool) error {
	existing, err := deps.Store.ScrollCIFailures(ctx, repo.Path, 1000)
	if err != nil {
		return err
	}
	pruned := 0
	for _, p := range existing {
		key := [2]string{payloadString(p.Payload, "pipeline_id"), payloadString(p.Payload, "job_id")}
		if current[key] {
			continue
		}
		if err := deps.Store.DeleteCIFailure(ctx, repo.Path, key[0], key[1]); err != nil {
			return err
		}
		pruned++
	}
	if deps.stats != nil {
		deps.stats.addPruned(pruned)
	}
	return nil
}

// payloadString reads a string payload field.
func payloadString(p map[string]any, key string) string {
	s, _ := p[key].(string)
	return s
}
