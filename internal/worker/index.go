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

// RepoDeps are the dependencies of processing a single repository (T041/T044).
type RepoDeps struct {
	VCSType string
	VCS     provider.VCSProvider // needed by the metadata lane (US4)
	Engine  *gitengine.Engine
	Embed   embed.Provider
	Store   *store.Store
	Cfg     *config.Config
	Logger  *slog.Logger
	stats   *summary // optional pass counters (T060)
}

// indexStore is the store surface of the index lane: a narrow interface so
// unit tests can inject failures/counters without Qdrant (T088).
type indexStore interface {
	DeleteCodeFile(ctx context.Context, repo, branch, filePath string) error
	DeleteBranch(ctx context.Context, repo, branch string) error
	DeleteCodeCommits(ctx context.Context, repo, branch string, shas []string) error
	Upsert(ctx context.Context, points []store.Point) error
}

// runIndex is the incremental code indexing lane (T041): on the
// WORKER_INTERVAL tick it processes registry repositories through the pool
// (WORKER_CONCURRENCY); errors do not break the loop (FR-014).
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

// indexAll indexes all known repositories through the pool; failures are
// isolated (FR-014), a summary is written after the pass (FR-020).
func indexAll(ctx context.Context, cfg *config.Config, deps RepoDeps, snap *snapshot) {
	repos := snap.get()
	if len(repos) == 0 {
		return
	}
	sum := newSummary("index")
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
			sum.countRepo(processSafely(ctx, deps.Logger, repo, "index", func() error {
				return ProcessRepoOnce(ctx, deps, repo)
			}))
		}(repo)
	}
	wg.Wait()
	sum.log(deps.Logger)
}

// ProcessRepoOnce is a full pass over a repository: local mirror (clone only
// when absent; fetching is owned by the mirror lane — new commits appear in
// the mirror on a WORKER_MIRROR_INTERVAL tick and are picked up by the next
// index pass) → branch snapshot → new commits (diff → blob → chunks →
// embeddings → upsert) → prune of rewritten commits and vanished branches.
// The branch progress is checkpointed after every indexed commit (T088): a
// failure/restart resumes from the last commit instead of the whole repository;
// the retry is safe — delete/upsert are idempotent (FR-014/FR-011). The sidecar
// is written only through updateState closures (T089): the index lane touches
// Branches only, so the meta/last_fetch_at sections of the other lanes survive.
func ProcessRepoOnce(ctx context.Context, deps RepoDeps, repo provider.Repo) error {
	return processRepoOnce(ctx, deps, repo, deps.Store)
}

// processRepoOnce is ProcessRepoOnce over the narrow indexStore interface.
func processRepoOnce(ctx context.Context, deps RepoDeps, repo provider.Repo, st indexStore) error {
	lg := log.WithRepo(deps.Logger, repo.Namespace+"/"+repo.Path)

	mirror, err := deps.Engine.EnsureLocalMirror(ctx, deps.VCSType, repo)
	if err != nil {
		return fmt.Errorf("mirror: %w", err)
	}
	statePath := gitengine.StatePath(mirror)
	state, err := LoadState(statePath)
	if err != nil {
		return err
	}
	refs, err := deps.Engine.Snapshot(ctx, mirror)
	if err != nil {
		return fmt.Errorf("branch snapshot: %w", err)
	}
	reader, err := deps.Engine.NewBlobReader(ctx, mirror)
	if err != nil {
		return err
	}
	defer reader.Close()

	finals := make(map[string]BranchState, len(refs))
	for branch, tip := range refs {
		bst, err := processBranch(ctx, deps, st, repo, mirror, branch, tip, state.Branches[branch], reader, lg, statePath)
		if err != nil {
			return fmt.Errorf("branch %s: %w", branch, err)
		}
		finals[branch] = bst
	}

	// Vanished branches → delete their points (data-model §2). The points go
	// first: if the deletion fails, the keys stay in the state for a retry.
	var vanished []string
	for branch := range state.Branches {
		if _, ok := refs[branch]; !ok {
			vanished = append(vanished, branch)
		}
	}
	for _, branch := range vanished {
		if err := st.DeleteBranch(ctx, repo.Path, branch); err != nil {
			return err
		}
		lg.Info("branch removed from the index", "branch", branch)
	}

	// T089: the single terminal write over the FRESH state — final branch
	// states and vanished keys only; meta/last_fetch_at written concurrently by
	// the other lanes are preserved (Branches is written by the index lane only).
	return updateState(statePath, func(fresh *MirrorState) {
		for branch, bst := range finals {
			fresh.Branches[branch] = bst
		}
		for _, branch := range vanished {
			delete(fresh.Branches, branch)
		}
	})
}

// processBranch indexes new branch commits — checkpointing the branch state
// after every commit (T088) — and prunes unreachable ones. It does not hold on
// to the state outside updateState closures: prev is a value copy.
func processBranch(ctx context.Context, deps RepoDeps, st indexStore, repo provider.Repo, mirror, branch, tip string, prev BranchState, reader *gitengine.BlobReader, lg *slog.Logger, statePath string) (BranchState, error) {
	allRev, err := deps.Engine.RevList(ctx, mirror, tip, 0)
	if err != nil {
		return BranchState{}, fmt.Errorf("rev-list: %w", err)
	}
	window := allRev
	if max := deps.Cfg.Worker.HistoryMaxCommits; max > 0 && len(window) > max {
		window = window[:max]
	}

	toIndex := newCommits(window, prev.IndexedCommits)
	done := make([]string, 0, len(toIndex))
	for _, sha := range toIndex { // oldest → newest
		if err := processCommit(ctx, deps, st, repo, mirror, branch, sha, reader, lg); err != nil {
			return BranchState{}, err
		}
		done = append(done, sha)
		// T088: persist the branch progress — a crash/restart resumes from here.
		if err := checkpointBranch(statePath, branch, tip, window, prev.IndexedCommits, done); err != nil {
			return BranchState{}, fmt.Errorf("state: %w", err)
		}
	}
	newCount := len(toIndex)

	// prune: indexed commits unreachable from the new tip
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
		if err := st.DeleteCodeCommits(ctx, repo.Path, branch, pruned); err != nil {
			return BranchState{}, err
		}
	}
	resumed := len(window) - newCount
	if deps.stats != nil {
		deps.stats.addIndexed(newCount)
		deps.stats.addPruned(len(pruned))
		deps.stats.addResumed(resumed) // T091
	}
	if newCount > 0 || len(pruned) > 0 {
		lg.Info("branch indexed", "branch", branch, "new_commits", newCount,
			"skipped_commits", resumed, "pruned", len(pruned))
	}

	return BranchState{Tip: tip, IndexedCommits: window, IndexedAt: time.Now().UTC()}, nil
}

// checkpointBranch persists the intermediate branch progress after a commit
// (T088): only st.Branches[branch] is touched inside the closure — the fresh
// state's meta/last_fetch_at and the other branches are preserved (the index
// lane is the only writer of Branches).
func checkpointBranch(statePath, branch, tip string, window, prev, done []string) error {
	idx := mergedIndexed(window, prev, done)
	at := time.Now().UTC()
	return updateState(statePath, func(st *MirrorState) {
		st.Branches[branch] = BranchState{Tip: tip, IndexedCommits: idx, IndexedAt: at}
	})
}

// mergedIndexed returns prev ∪ done in the window order (newest→oldest); prev
// shas that left the window are kept for the prune bookkeeping on resume.
func mergedIndexed(window, prev, done []string) []string {
	indexed := make(map[string]bool, len(prev)+len(done))
	for _, sha := range prev {
		indexed[sha] = true
	}
	for _, sha := range done {
		indexed[sha] = true
	}
	inWindow := make(map[string]bool, len(window))
	out := make([]string, 0, len(indexed))
	for _, sha := range window {
		inWindow[sha] = true
		if indexed[sha] {
			out = append(out, sha)
		}
	}
	for _, sha := range prev {
		if !inWindow[sha] {
			out = append(out, sha)
		}
	}
	return out
}

// newCommits selects window commits (newest→oldest) missing from indexed;
// the result order is oldest→newest (indexing order). A repeated pass with
// a full indexed set yields no commits — resume/idempotency (FR-006/011).
func newCommits(window, indexed []string) []string {
	if len(window) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(indexed))
	for _, sha := range indexed {
		seen[sha] = true
	}
	var out []string
	for i := len(window) - 1; i >= 0; i-- {
		if !seen[window[i]] {
			out = append(out, window[i])
		}
	}
	return out
}

// processCommit indexes the changed files of a commit: old file points are
// deleted, eligible blobs are chunked and written anew.
func processCommit(ctx context.Context, deps RepoDeps, st indexStore, repo provider.Repo, mirror, branch, sha string, reader *gitengine.BlobReader, lg *slog.Logger) error {
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
		if err := st.DeleteCodeFile(ctx, repo.Path, branch, ch.Path); err != nil {
			return err
		}
		if ch.Status == "D" {
			continue
		}
		file, ok := byPath[ch.Path]
		if !ok {
			continue // a large file or not a blob — not indexed
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
			return fmt.Errorf("embeddings %s: %w", ch.Path, err)
		}
		points := make([]store.Point, len(chunks))
		for i, c := range chunks {
			points[i] = store.Point{
				ID:      store.CodeChunkID(deps.VCSType, repo.Namespace, repo.Path, branch, sha, ch.Path, c.Index),
				Vector:  vecs[i],
				Payload: store.CodePayload(deps.VCSType, repo.Namespace, repo.Path, branch, sha, ch.Path, c.Text, c.Index),
			}
		}
		if err := st.Upsert(ctx, points); err != nil {
			return err
		}
	}
	lg.Debug("commit indexed", "branch", branch, "commit", sha, "changes", len(changes))
	return nil
}
