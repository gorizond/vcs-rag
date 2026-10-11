package worker

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gorizond/vcs-rag/internal/gitengine"
	"github.com/gorizond/vcs-rag/internal/provider"
	"github.com/gorizond/vcs-rag/internal/store"
)

// refreshStatus is the refresh_status KV value (T109/T112 wire contract; the
// MCP read model repeats it in internal/mcp/refresh.go).
type refreshStatus struct {
	State          string    `json:"state"` // in_progress | done | failed
	RequestID      string    `json:"request_id,omitempty"`
	StartedAt      time.Time `json:"started_at,omitempty"`
	FinishedAt     time.Time `json:"finished_at,omitempty"`
	CommitsIndexed int       `json:"commits_indexed"`
	MetaUpdated    int       `json:"meta_updated"`
	Errors         []string  `json:"errors,omitempty"`
	DurationMS     int64     `json:"duration_ms"`
}

// RefreshResult is the outcome of one on-demand repository refresh (T111).
type RefreshResult struct {
	CommitsIndexed int
	MetaUpdated    int
	Errors         []string // per-phase errors; the phases are best-effort
	Duration       time.Duration
}

// errRefreshTransient marks retryable resolve failures (an empty discovery
// snapshot — the listing has not produced data yet).
var errRefreshTransient = errors.New("transient")

// metaAutoFresh is the freshness window of the metadata phase for "auto"
// refreshes (T135): a non-user trigger skips metadata when it ran recently.
const metaAutoFresh = time.Hour

// refreshFetchTimeout bounds the network fetch phase of an on-demand refresh
// (T142): a hung git transport must not pin the single-consumer lane forever.
// A timeout is an ordinary phase error — the task is Nak-ed and retried with
// the consumer backoff, so the retry semantics are unchanged.
var refreshFetchTimeout = 15 * time.Minute

// RefreshOnce performs an on-demand refresh of one repository (T111): fetch
// (mirror-lane semantics) → index new commits (per-commit checkpoints, T088) →
// metadata. An error of a phase does not cancel the next one — it is collected
// in Errors. The whole composition runs under the per-repo lock (T110); the
// NATS consumer is the production caller (T112). The idempotent checkpointed
// pipelines make a redelivery safe. The exported wrapper is an explicit request
// (origin ""): it always refreshes metadata.
func RefreshOnce(ctx context.Context, deps RepoDeps, repo provider.Repo) RefreshResult {
	return refreshOnce(ctx, deps, deps.Store, deps.Store, repo, "")
}

// refreshOnce is RefreshOnce over the narrow store interfaces (fakes in tests);
// origin "auto" marks a non-user trigger (T135).
func refreshOnce(ctx context.Context, deps RepoDeps, idx indexStore, meta metaStore, repo provider.Repo, origin string) RefreshResult {
	start := time.Now()
	res := RefreshResult{}
	_ = withRepoLock(repoSlug(repo.Namespace+"/"+repo.Path), func() error {
		statePath := gitengine.StatePath(deps.Engine.MirrorPath(deps.VCSType, repo))
		before, err := LoadState(statePath)
		if err != nil {
			res.Errors = append(res.Errors, "state: "+err.Error())
			before = &MirrorState{}
		}

		// phase 1: fetch — the refresh owns network access (mirror lane semantics);
		// the phase is bounded by refreshFetchTimeout (T142).
		fetchCtx, cancel := context.WithTimeout(ctx, refreshFetchTimeout)
		_, errFetch := deps.Engine.EnsureMirror(fetchCtx, deps.VCSType, repo)
		cancel()
		if errFetch != nil {
			res.Errors = append(res.Errors, "mirror: "+errFetch.Error())
		}
		// phase 2: index new commits.
		if err := processRepoOnce(ctx, deps, repo, idx); err != nil {
			res.Errors = append(res.Errors, "index: "+err.Error())
		}
		if after, err := LoadState(statePath); err == nil {
			res.CommitsIndexed = indexedDelta(before.Branches, after.Branches)
		}
		// phase 2b (T020): graph extraction for the refreshed repository — the
		// same entry point as the graph lane, on the freshly fetched mirror.
		// Best-effort like the other phases: a failure is collected, not fatal.
		if gs := refreshGraphStore(deps); gs != nil {
			if _, err := processGraphRepo(ctx, deps.Cfg, deps, gs, repo, int(time.Now().Unix())); err != nil {
				res.Errors = append(res.Errors, "graph: "+err.Error())
			}
		}
		// phase 3: metadata (best-effort). T135: an "auto" refresh skips it when
		// the repository was metadata-synced recently — the background metadata
		// lane covers it; an explicit request always refreshes metadata.
		metaBefore, err := LoadState(statePath)
		switch {
		case err != nil:
			res.Errors = append(res.Errors, "state: "+err.Error())
		case origin == "auto" && !metaBefore.MetaSyncedAt.IsZero() &&
			time.Since(metaBefore.MetaSyncedAt) < metaAutoFresh:
			// skip: the metadata phase is fresh enough
		default:
			if err := processMetadataOnce(ctx, deps, repo, meta); err != nil {
				res.Errors = append(res.Errors, "metadata: "+err.Error())
			}
			if metaAfter, err := LoadState(statePath); err == nil {
				res.MetaUpdated = metaDelta(metaBefore.Meta, metaAfter.Meta)
			}
		}
		return nil
	})
	res.Duration = time.Since(start)
	return res
}

// refreshGraphStore derives the graph store from the worker store the refresh
// already holds (T020): one client, the config-named payload-only collection.
// Nil — the graph phase is skipped — without a worker store (narrow unit tests
// use fake index/meta stores) or without a config.
func refreshGraphStore(deps RepoDeps) *store.GraphStore {
	if deps.Store == nil || deps.Cfg == nil {
		return nil
	}
	return store.NewGraphStore(deps.Store, deps.Cfg.Qdrant.GraphCollection)
}

// indexedDelta counts the commits that appeared in the branch state since the
// previous snapshot (checkpointed partial progress included).
func indexedDelta(before, after map[string]BranchState) int {
	n := 0
	for branch, st := range after {
		known := make(map[string]bool, len(before[branch].IndexedCommits))
		for _, sha := range before[branch].IndexedCommits {
			known[sha] = true
		}
		for _, sha := range st.IndexedCommits {
			if !known[sha] {
				n++
			}
		}
	}
	return n
}

// metaDelta counts new/changed meta keys of one metadata phase.
func metaDelta(before, after map[string]int64) int {
	n := 0
	for key, at := range after {
		if old, ok := before[key]; !ok || old != at {
			n++
		}
	}
	return n
}

// resolveRepo resolves a requested repository against the discovery snapshot
// (T111): an exact "namespace/path" match, otherwise a unique suffix match
// (e.g. "fixture" or "grp/fixture"); no match and an ambiguous suffix are
// errors — the latter lists the candidates.
func resolveRepo(repos []provider.Repo, want string) (provider.Repo, error) {
	want = strings.Trim(strings.TrimSpace(want), "/")
	if want == "" {
		return provider.Repo{}, errors.New("refresh: empty repository name")
	}
	var exact *provider.Repo
	var suffix []provider.Repo
	for i := range repos {
		r := repos[i]
		full := r.Namespace + "/" + r.Path
		switch {
		case full == want:
			exact = &repos[i]
		case strings.HasSuffix(full, "/"+want):
			suffix = append(suffix, r)
		}
	}
	if exact != nil {
		return *exact, nil
	}
	switch len(suffix) {
	case 0:
		return provider.Repo{}, fmt.Errorf("refresh: repository %q not found in the discovery snapshot", want)
	case 1:
		return suffix[0], nil
	default:
		names := make([]string, 0, len(suffix))
		for _, r := range suffix {
			names = append(names, r.Namespace+"/"+r.Path)
		}
		sort.Strings(names)
		return provider.Repo{}, fmt.Errorf("refresh: repository %q is ambiguous: %s", want, strings.Join(names, ", "))
	}
}
