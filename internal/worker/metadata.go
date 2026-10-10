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

// runMetadata — линия метаданных (T054): по тикеру WORKER_INTERVAL
// обрабатывает репозитории реестра через пул; ошибки не прерывают цикл (FR-014).
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

// metadataAll обрабатывает метаданные всех известных репозиториев через пул.
func metadataAll(ctx context.Context, cfg *config.Config, deps RepoDeps, snap *snapshot) {
	repos := snap.get()
	if len(repos) == 0 {
		return
	}
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
			if err := ProcessMetadataOnce(ctx, deps, repo); err != nil {
				if ctx.Err() != nil {
					return
				}
				deps.Logger.Error("обновление метаданных не удалось", "repo", repo.Namespace+"/"+repo.Path, "err", err)
			}
		}(repo)
	}
	wg.Wait()
}

// ProcessMetadataOnce — один проход метаданных репозитория: issues + MR/PR
// (upsert по детерминированным ID обновляет state/text), затем CI-фейлы
// (tail ≤ ciTailLines строк) и prune ci-точек, исчезнувших из текущего окна.
// Выделен для e2e-теста (T055).
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

	// --- CI: упавшие jobs текущего окна ---
	pipelines, err := deps.VCS.ListPipelines(ctx, repo)
	if err != nil {
		return fmt.Errorf("пайплайны: %w", err)
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
				// лог недоступен — точка всё равно полезнее без хвоста;
				// ошибка не прерывает проход (FR-014).
				lg.Error("хвост лога job не получен", "pipeline", p.ID, "job", j.ID, "err", err)
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

	lg.Info("метаданные обновлены", "issues", len(issues), "prs", len(prs), "ci_failures", len(ciItems))
	return nil
}

// upsertItems эмбеддит тексты (батчи внутри провайдера) и пишет точки.
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
		return fmt.Errorf("эмбеддинги метаданных: %w", err)
	}
	points := make([]store.Point, len(items))
	for i, it := range items {
		points[i] = store.Point{ID: it.ID, Vector: vecs[i], Payload: it.Payload}
	}
	return deps.Store.Upsert(ctx, points)
}

// pruneCIFailures удаляет ci-точки репозитория, которых нет в текущем
// наборе (pipeline_id, job_id) — например, после перезапуска job'а в success.
func pruneCIFailures(ctx context.Context, deps RepoDeps, repo provider.Repo, current map[[2]string]bool) error {
	existing, err := deps.Store.ScrollCIFailures(ctx, repo.Path, 1000)
	if err != nil {
		return err
	}
	for _, p := range existing {
		key := [2]string{payloadString(p.Payload, "pipeline_id"), payloadString(p.Payload, "job_id")}
		if current[key] {
			continue
		}
		if err := deps.Store.DeleteCIFailure(ctx, repo.Path, key[0], key[1]); err != nil {
			return err
		}
	}
	return nil
}

// payloadString читает строковое поле payload.
func payloadString(p map[string]any, key string) string {
	s, _ := p[key].(string)
	return s
}
