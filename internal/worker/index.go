package worker

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/gorizond/vcs-rag/internal/chunk"
	"github.com/gorizond/vcs-rag/internal/config"
	"github.com/gorizond/vcs-rag/internal/embed"
	"github.com/gorizond/vcs-rag/internal/gitengine"
	"github.com/gorizond/vcs-rag/internal/log"
	"github.com/gorizond/vcs-rag/internal/provider"
	"github.com/gorizond/vcs-rag/internal/store"
)

// RepoDeps — зависимости обработки одного репозитория (T041/T044).
type RepoDeps struct {
	VCSType string
	VCS     provider.VCSProvider // нужен линии метаданных (US4)
	Engine  *gitengine.Engine
	Embed   embed.Provider
	Store   *store.Store
	Cfg     *config.Config
	Logger  *slog.Logger
}

// runIndex — линия инкрементальной индексации кода (T041): по тикеру
// WORKER_INTERVAL обрабатывает репозитории реестра через пул
// WORKER_CONCURRENCY; ошибки не прерывают цикл (FR-014).
func runIndex(ctx context.Context, cfg *config.Config, deps RepoDeps, snap *snapshot) error {
	deps.Logger = log.WithPhase(deps.Logger, "index")
	indexAll(ctx, cfg, deps, snap)

	ticker := time.NewTicker(cfg.Worker.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			indexAll(ctx, cfg, deps, snap)
		}
	}
}

// indexAll индексирует все известные репозитории через пул.
func indexAll(ctx context.Context, cfg *config.Config, deps RepoDeps, snap *snapshot) {
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
			if err := ProcessRepoOnce(ctx, deps, repo); err != nil {
				if ctx.Err() != nil {
					return
				}
				deps.Logger.Error("индексация репозитория не удалась", "repo", repo.Namespace+"/"+repo.Path, "err", err)
			}
		}(repo)
	}
	wg.Wait()
}

// ProcessRepoOnce — полный проход по репозиторию: зеркало → снимок веток →
// новые коммиты (diff → blob → чанки → эмбеддинги → upsert) → prune
// rewritten-коммитов и исчезнувших веток. Состояние сохраняется только
// после успеха (FR-014): при ошибке повтор на следующем проходе безопасен —
// delete/upsert идемпотентны (FR-011).
func ProcessRepoOnce(ctx context.Context, deps RepoDeps, repo provider.Repo) error {
	lg := log.WithRepo(deps.Logger, repo.Namespace+"/"+repo.Path)

	mirror, err := deps.Engine.EnsureMirror(ctx, deps.VCSType, repo)
	if err != nil {
		return fmt.Errorf("зеркало: %w", err)
	}
	statePath := gitengine.StatePath(mirror)
	st, err := LoadState(statePath)
	if err != nil {
		return err
	}
	refs, err := deps.Engine.Snapshot(ctx, mirror)
	if err != nil {
		return fmt.Errorf("снимок веток: %w", err)
	}
	reader, err := deps.Engine.NewBlobReader(ctx, mirror)
	if err != nil {
		return err
	}
	defer reader.Close()

	for branch, tip := range refs {
		if err := processBranch(ctx, deps, repo, mirror, branch, tip, st, reader, lg); err != nil {
			return fmt.Errorf("ветка %s: %w", branch, err)
		}
	}

	// Исчезнувшие ветки → удаление всех их точек (data-model §2).
	for branch := range st.Branches {
		if _, ok := refs[branch]; ok {
			continue
		}
		if err := deps.Store.DeleteBranch(ctx, repo.Path, branch); err != nil {
			return err
		}
		delete(st.Branches, branch)
		lg.Info("ветка удалена из индекса", "branch", branch)
	}

	return SaveState(statePath, st)
}

// processBranch индексирует новые коммиты ветки и prune'ит недостижимые.
func processBranch(ctx context.Context, deps RepoDeps, repo provider.Repo, mirror, branch, tip string, st *MirrorState, reader *gitengine.BlobReader, lg *slog.Logger) error {
	prev := st.Branches[branch]
	allRev, err := deps.Engine.RevList(ctx, mirror, tip, 0)
	if err != nil {
		return fmt.Errorf("rev-list: %w", err)
	}
	window := allRev
	if max := deps.Cfg.Worker.HistoryMaxCommits; max > 0 && len(window) > max {
		window = window[:max]
	}

	indexed := make(map[string]bool, len(prev.IndexedCommits))
	for _, sha := range prev.IndexedCommits {
		indexed[sha] = true
	}
	newCount := 0
	for i := len(window) - 1; i >= 0; i-- { // oldest → newest
		sha := window[i]
		if indexed[sha] {
			continue
		}
		if err := processCommit(ctx, deps, repo, mirror, branch, sha, reader, lg); err != nil {
			return err
		}
		newCount++
	}

	// prune: проиндексированные коммиты, недостижимые из нового tip
	// (force-push/rewrite, FR-016).
	inTip := make(map[string]bool, len(allRev))
	for _, sha := range allRev {
		inTip[sha] = true
	}
	var pruned []string
	for _, sha := range prev.IndexedCommits {
		if !inTip[sha] {
			pruned = append(pruned, sha)
		}
	}
	if len(pruned) > 0 {
		if err := deps.Store.DeleteCodeCommits(ctx, repo.Path, branch, pruned); err != nil {
			return err
		}
	}
	if newCount > 0 || len(pruned) > 0 {
		lg.Info("ветка проиндексирована", "branch", branch, "new_commits", newCount, "pruned", len(pruned))
	}

	st.Branches[branch] = BranchState{
		Tip:            tip,
		IndexedCommits: window,
		IndexedAt:      time.Now().UTC(),
	}
	return nil
}

// processCommit индексирует изменённые файлы коммита: старые точки файла
// удаляются, подходящие blob'ы чанкуются и пишутся заново.
func processCommit(ctx context.Context, deps RepoDeps, repo provider.Repo, mirror, branch, sha string, reader *gitengine.BlobReader, lg *slog.Logger) error {
	changes, err := deps.Engine.DiffCommit(ctx, mirror, sha)
	if err != nil {
		return fmt.Errorf("diff %s: %w", sha, err)
	}
	tree, err := deps.Engine.TreeFiles(ctx, mirror, sha, deps.Cfg.Worker.MaxFileBytes)
	if err != nil {
		return fmt.Errorf("tree %s: %w", sha, err)
	}
	byPath := make(map[string]gitengine.TreeFile, len(tree))
	for _, f := range tree {
		byPath[f.Path] = f
	}

	for _, ch := range changes {
		if err := deps.Store.DeleteCodeFile(ctx, repo.Path, branch, ch.Path); err != nil {
			return err
		}
		if ch.Status == "D" {
			continue
		}
		file, ok := byPath[ch.Path]
		if !ok {
			continue // крупный файл или не blob — не индексируется
		}
		content, found, err := reader.ReadBlob(file.SHA)
		if err != nil {
			return fmt.Errorf("blob %s: %w", ch.Path, err)
		}
		if !found || gitengine.IsBinary(content) {
			continue
		}
		chunks := chunk.Split(ch.Path, content)
		if len(chunks) == 0 {
			continue
		}
		texts := make([]string, len(chunks))
		for i, c := range chunks {
			texts[i] = c.Text
		}
		vecs, err := deps.Embed.Embed(ctx, texts)
		if err != nil {
			return fmt.Errorf("эмбеддинги %s: %w", ch.Path, err)
		}
		points := make([]store.Point, len(chunks))
		for i, c := range chunks {
			points[i] = store.Point{
				ID:      store.CodeChunkID(deps.VCSType, repo.Namespace, repo.Path, branch, sha, ch.Path, c.Index),
				Vector:  vecs[i],
				Payload: store.CodePayload(deps.VCSType, repo.Namespace, repo.Path, branch, sha, ch.Path, c.Text, c.Index),
			}
		}
		if err := deps.Store.Upsert(ctx, points); err != nil {
			return err
		}
	}
	lg.Debug("коммит проиндексирован", "branch", branch, "commit", sha, "changes", len(changes))
	return nil
}
