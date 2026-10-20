package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gorizond/vcs-rag/internal/config"
	"github.com/gorizond/vcs-rag/internal/gitengine"
	"github.com/gorizond/vcs-rag/internal/log"
	"github.com/gorizond/vcs-rag/internal/provider"
)

// Repo-GC parameters (T140). The GC lane is the only writer of the registry;
// discovery is the source of truth — a repository is deleted only after it is
// absent from two consecutive listings and the grace period has passed.
const (
	gcRegistryName        = ".gc_registry.json"
	gcGraceStreak         = 2                // missing passes before a candidate
	gcGracePeriod         = 10 * time.Minute // and at least this long since first_missing_at
	gcMaxDeletesPerPass   = 10
	gcAnomalyFreezePasses = 3                // anomaly passes before streaks resume
	gcAnomalyMinKnown     = 10               // the >30% drop guard needs this many known repos
	gcAnomalyDropPct      = 70               // a snapshot below 70% of the previous size
	gcAnomalyStaleMin     = 10 * time.Minute // stale snapshot floor: max(3×INTERVAL, this)
)

// gcEntry is one repository tracked by the registry: the exact payload pair
// (namespace, path) plus the disappearance bookkeeping.
type gcEntry struct {
	Namespace      string    `json:"namespace"`
	Path           string    `json:"path"`
	LastSeenAt     time.Time `json:"last_seen_at"`
	FirstMissingAt time.Time `json:"first_missing_at,omitempty"`
	MissStreak     int       `json:"miss_streak"`
}

// gcRegistry is the persistent repo-GC state; single writer — the GC lane.
type gcRegistry struct {
	Entries map[string]*gcEntry `json:"entries"` // key: namespace/path
}

// gcKey is the registry key of a repository.
func gcKey(namespace, path string) string { return namespace + "/" + path }

// loadGCRegistry reads the registry; a missing file yields an empty registry.
func loadGCRegistry(path string) (*gcRegistry, error) {
	reg := &gcRegistry{Entries: map[string]*gcEntry{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return reg, nil
	}
	if err != nil {
		return nil, fmt.Errorf("gc registry %s: %w", path, err)
	}
	if err := json.Unmarshal(data, reg); err != nil {
		return nil, fmt.Errorf("gc registry %s: %w", path, err)
	}
	if reg.Entries == nil {
		reg.Entries = map[string]*gcEntry{}
	}
	return reg, nil
}

// save writes the registry atomically: tmp file + rename (single writer).
func (r *gcRegistry) save(path string) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("gc registry %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("gc registry %s: %w", path, err)
	}
	return nil
}

// gcStore is the narrow store surface of the GC lane (fake in tests).
type gcStore interface {
	CountRepoPoints(ctx context.Context, vcsType, groupOrg, repo string) (uint64, error)
	DeleteRepoPoints(ctx context.Context, vcsType, groupOrg, repo string) error
}

// gcRunner is the in-memory state of the repo-GC lane: the registry (single
// writer), the anomaly-freeze counter and the previous known snapshot size.
type gcRunner struct {
	cfg  *config.Config
	deps RepoDeps
	snap *snapshot
	reg  *gcRegistry
	now  func() time.Time

	freeze    int
	prevKnown int
	seeded    bool
}

func newGCRunner(cfg *config.Config, deps RepoDeps, snap *snapshot, reg *gcRegistry) *gcRunner {
	return &gcRunner{cfg: cfg, deps: deps, snap: snap, reg: reg, now: time.Now}
}

func (r *gcRunner) registryPath() string {
	return filepath.Join(r.cfg.Worker.MirrorsDir, gcRegistryName)
}

// runGC is the repo-GC lane (T140): an adaptive paced loop (ceiling
// WORKER_GC_INTERVAL) that deletes repositories vanished from the discovery —
// discovery is the source of truth. The dry-run default (WORKER_GC_DRY_RUN)
// keeps it observational until the kill-switch is disabled explicitly.
func runGC(ctx context.Context, cfg *config.Config, deps RepoDeps, snap *snapshot) error {
	deps.Logger = log.WithPhase(deps.Logger, "gc")
	p := newPacerWithLimits(cfg.Worker.GCInterval, cfg.Worker.RetryFloor, cfg.Worker.RetryCap)
	// T129: a random start U[0, 2s] avoids a boot herd across lanes.
	if err := p.sleep(ctx, p.startDelay()); err != nil {
		return nil
	}
	reg, err := loadGCRegistry(filepath.Join(cfg.Worker.MirrorsDir, gcRegistryName))
	if err != nil {
		deps.Logger.Error("gc registry unreadable, starting empty", "err", err)
		reg = &gcRegistry{Entries: map[string]*gcEntry{}}
	}
	runner := newGCRunner(cfg, deps, snap, reg)
	for {
		sum := runner.pass(ctx, deps.Store)
		busy := sum.gcDeletedRepos.Load() > 0
		delay := p.next(busy, false, 0)
		sum.withPace(delay, p.reason)
		sum.log(deps.Logger)
		if err := p.sleep(ctx, delay); err != nil {
			return nil
		}
	}
}

// pass performs one GC pass: anomaly guard → seed (first pass) → snapshot
// upsert → grace/streak evaluation → try-locked deletion of ≤10 candidates.
func (r *gcRunner) pass(ctx context.Context, st gcStore) *summary {
	sum := newSummary("gc")
	now := r.now()
	repos := r.snap.get()

	if reason := r.anomalyReason(repos, now); reason != "" {
		sum.gcAnomalies.Add(1)
		if r.freeze < gcAnomalyFreezePasses {
			r.freeze++
			r.deps.Logger.Warn("gc skipped: anomaly", "reason", reason, "freeze", r.freeze)
			return sum // streaks are not moved while the anomaly persists
		}
		r.deps.Logger.Warn("gc anomaly persists: streak accounting resumes", "reason", reason)
	} else {
		r.freeze = 0
	}

	// first non-anomalous pass: seed mirror entries matched by a known namespace.
	if !r.seeded {
		r.seedMirrors(now)
		r.seeded = true
	}

	// snapshot entries: upsert and reset the disappear bookkeeping on return.
	inSnapshot := make(map[string]bool, len(repos))
	for _, repo := range repos {
		key := gcKey(repo.Namespace, repo.Path)
		inSnapshot[key] = true
		e, ok := r.reg.Entries[key]
		if !ok {
			e = &gcEntry{Namespace: repo.Namespace, Path: repo.Path}
			r.reg.Entries[key] = e
		}
		if e.MissStreak > 0 || !e.FirstMissingAt.IsZero() {
			e.MissStreak = 0
			e.FirstMissingAt = time.Time{}
		}
		e.LastSeenAt = now
		sum.gcKnown.Add(1)
	}

	// vanished entries: grace, then candidates.
	nsSeen := make(map[string]bool, len(repos))
	for _, repo := range repos {
		nsSeen[repo.Namespace] = true
	}
	nsVanished := map[string]int{}
	var candidates []*gcEntry
	for _, e := range r.reg.Entries {
		if inSnapshot[gcKey(e.Namespace, e.Path)] {
			continue
		}
		sum.gcVanished.Add(1)
		if e.FirstMissingAt.IsZero() {
			e.FirstMissingAt = now
		}
		e.MissStreak++
		if e.MissStreak >= gcGraceStreak && now.Sub(e.FirstMissingAt) >= gcGracePeriod {
			candidates = append(candidates, e)
		} else {
			sum.gcSuppressedGrace.Add(1)
		}
		if !nsSeen[e.Namespace] {
			nsVanished[e.Namespace]++
		}
	}
	for ns, n := range nsVanished {
		r.deps.Logger.Warn("gc: entire namespace vanished from discovery", "namespace", ns, "repos", n)
	}

	// oldest disappearance first; the per-pass cap protects against a mass event.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].FirstMissingAt.Before(candidates[j].FirstMissingAt)
	})
	if len(candidates) > gcMaxDeletesPerPass {
		sum.gcSuppressedCap.Add(int64(len(candidates) - gcMaxDeletesPerPass))
		candidates = candidates[:gcMaxDeletesPerPass]
	}

	dry := r.cfg.Worker.GCDryRun
	for _, e := range candidates {
		name := gcKey(e.Namespace, e.Path)
		ran, err := tryRepoLock(repoSlug(name), func() error {
			n, err := st.CountRepoPoints(ctx, r.deps.VCSType, e.Namespace, e.Path)
			if err != nil {
				return fmt.Errorf("count: %w", err)
			}
			if dry {
				r.deps.Logger.Info("gc would-delete", "repo", name, "points", n)
				return nil
			}
			if err := st.DeleteRepoPoints(ctx, r.deps.VCSType, e.Namespace, e.Path); err != nil {
				return fmt.Errorf("delete: %w", err)
			}
			r.deps.Logger.Info("gc deleted", "repo", name, "points", n)
			// the points are gone — the files are best-effort from here on.
			r.removeMirrorFiles(e, name)
			delete(r.reg.Entries, name)
			sum.gcDeletedRepos.Add(1)
			sum.gcDeletedPoints.Add(int64(n))
			return nil
		})
		switch {
		case !ran:
			sum.gcSuppressedBusy.Add(1)
			r.deps.Logger.Info("gc candidate skipped: repository is busy", "repo", name)
		case err != nil:
			// the entry stays: the next pass retries the deletion.
			sum.failed.Add(1)
			r.deps.Logger.Warn("gc candidate skipped", "repo", name, "err", err)
		case dry:
			sum.gcDryRun.Add(1)
		}
	}

	r.prevKnown = len(repos)
	if err := r.reg.save(r.registryPath()); err != nil {
		sum.failed.Add(1)
		r.deps.Logger.Error("gc registry save failed", "err", err)
	}
	return sum
}

// anomalyReason classifies a defensive freeze: an empty snapshot, a >30% drop
// (with at least 10 known repositories) or a stale snapshot. The GC must not
// nuke the index because the provider had a bad day.
func (r *gcRunner) anomalyReason(repos []provider.Repo, now time.Time) string {
	if len(repos) == 0 {
		if len(r.reg.Entries) > 0 {
			return "discovery snapshot is empty"
		}
		return "" // nothing to protect yet
	}
	if r.prevKnown >= gcAnomalyMinKnown && len(repos)*100 < r.prevKnown*gcAnomalyDropPct {
		return fmt.Sprintf("snapshot dropped to %d from %d repositories (>30%%)", len(repos), r.prevKnown)
	}
	maxStale := 3 * r.cfg.Worker.Interval
	if maxStale < gcAnomalyStaleMin {
		maxStale = gcAnomalyStaleMin
	}
	if at := r.snap.at(); at.IsZero() || now.Sub(at) > maxStale {
		return "discovery snapshot is stale"
	}
	return ""
}

// seedMirrors adds entries for mirrors whose namespace is known (T140): such
// mirrors get the normal grace instead of an immediate deletion; a mirror of
// an unknown namespace is an orphan — WARN only, no entry, no deletion.
func (r *gcRunner) seedMirrors(now time.Time) {
	root := filepath.Join(r.cfg.Worker.MirrorsDir, r.deps.VCSType)
	dirs, err := os.ReadDir(root)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			r.deps.Logger.Warn("gc mirror scan failed", "dir", root, "err", err)
		}
		return
	}
	nsByDir := map[string]string{}
	for _, repo := range r.snap.get() {
		nsByDir[strings.ReplaceAll(repo.Namespace, "/", "_")] = repo.Namespace
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		ns, known := nsByDir[d.Name()]
		subs, err := os.ReadDir(filepath.Join(root, d.Name()))
		if err != nil {
			r.deps.Logger.Warn("gc mirror scan failed", "dir", filepath.Join(root, d.Name()), "err", err)
			continue
		}
		for _, sub := range subs {
			path, ok := strings.CutSuffix(sub.Name(), ".git")
			if !ok {
				continue
			}
			if !known {
				r.deps.Logger.Warn("gc: orphan mirror (namespace is not in the discovery)",
					"namespace_dir", d.Name(), "repo", path)
				continue
			}
			key := gcKey(ns, path)
			if _, ok := r.reg.Entries[key]; !ok {
				r.reg.Entries[key] = &gcEntry{Namespace: ns, Path: path, LastSeenAt: now}
			}
		}
	}
}

// removeMirrorFiles deletes the local mirror and its state sidecar (best-effort,
// T140): the Qdrant points are already gone, a disk error is only a WARN.
func (r *gcRunner) removeMirrorFiles(e *gcEntry, name string) {
	mirror := r.deps.Engine.MirrorPath(r.deps.VCSType, provider.Repo{Namespace: e.Namespace, Path: e.Path})
	if err := os.RemoveAll(mirror); err != nil {
		r.deps.Logger.Warn("gc mirror removal failed", "repo", name, "err", err)
	}
	for _, p := range []string{gitengine.StatePath(mirror), gitengine.StatePath(mirror) + ".tmp"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			r.deps.Logger.Warn("gc state removal failed", "path", p, "err", err)
		}
	}
}
