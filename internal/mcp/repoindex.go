package mcp

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// Repository-list cache of one instance (T132): the search tools resolve a
// short repo form ("nats") to the full path ("gorizond/devops/nats") without
// extra provider API calls. The list lives in memory: TTL 60m ±10%, one
// in-flight fetch (single-flight), stale-on-error with a 5m retry cooldown.
const (
	repoIndexTTL        = 60 * time.Minute
	repoIndexJitter     = 0.1 // TTL × U[0.9, 1.1]
	repoIndexTimeout    = 20 * time.Second
	repoIndexRetryAfter = 5 * time.Minute
	repoIndexMaxDrop    = 2 // suffix levels: exact, drop 1, drop 2 leading segments
	repoMaxCandidates   = 16
)

// repoLister is the narrow provider surface needed by the index.
type repoLister interface {
	ListRepositories(ctx context.Context) ([]provider.Repo, error)
}

// repoIndex caches the full repository names of the instance.
type repoIndex struct {
	lister  repoLister
	timeout time.Duration
	now     func() time.Time
	rnd     func() float64 // U[0,1), TTL jitter

	mu      sync.Mutex
	names   []string  // last good sorted list
	exp     time.Time // names are fresh until exp
	lastErr time.Time // last failed fetch (retry cooldown)
	loading *repoLoad // non-nil while one fetch is in flight
}

// repoLoad is one shared fetch handed to concurrent callers (single-flight).
type repoLoad struct {
	done  chan struct{}
	names []string
	err   error
}

// newRepoIndex creates the index; the fetch happens lazily on the first resolve.
func newRepoIndex(l repoLister) *repoIndex {
	return &repoIndex{lister: l, timeout: repoIndexTimeout, now: time.Now, rnd: rand.Float64}
}

// resolve returns the full repository names matching raw:
//   - raw == exact full name → that one;
//   - otherwise the first suffix level (drop 1, then 2 leading segments) that
//     yields a non-empty match set; several matches at that level → ambiguous
//     (all of them are returned, sorted);
//   - no matches / fetch error without a cached list → nothing.
func (r *repoIndex) resolve(ctx context.Context, raw string) ([]string, error) {
	raw = strings.Trim(strings.TrimSpace(raw), "/")
	if raw == "" {
		return nil, nil
	}
	names, err := r.ensure(ctx)
	if err != nil {
		return nil, err
	}
	return matchRepo(names, raw), nil
}

// ensure returns the cached list, refreshing it when stale. Concurrent callers
// share one fetch; a failing fetch is retried no sooner than in 5 minutes,
// serving the previous list meanwhile (stale-on-error).
func (r *repoIndex) ensure(ctx context.Context) ([]string, error) {
	r.mu.Lock()
	now := r.now()
	if r.names != nil && now.Before(r.exp) {
		out := r.names
		r.mu.Unlock()
		return out, nil
	}
	if ld := r.loading; ld != nil {
		r.mu.Unlock()
		<-ld.done
		return ld.names, ld.err
	}
	if !r.lastErr.IsZero() && now.Sub(r.lastErr) < repoIndexRetryAfter {
		if r.names == nil {
			r.mu.Unlock()
			return nil, fmt.Errorf("repo index: no list yet, the last error is within the %v retry cooldown", repoIndexRetryAfter)
		}
		out := r.names
		r.mu.Unlock()
		return out, nil
	}
	ld := &repoLoad{done: make(chan struct{})}
	r.loading = ld
	r.mu.Unlock()

	names, err := r.fetch(ctx)

	r.mu.Lock()
	if err != nil {
		r.lastErr = r.now()
		// stale-on-error: keep serving the last good list when there is one.
		ld.names, ld.err = r.names, nil
		if ld.names == nil {
			ld.err = err
		}
	} else {
		r.names = names
		r.lastErr = time.Time{}
		ttl := float64(repoIndexTTL) * (1 - repoIndexJitter + 2*repoIndexJitter*r.rnd())
		r.exp = r.now().Add(time.Duration(ttl))
		ld.names = names
	}
	r.loading = nil
	close(ld.done)
	r.mu.Unlock()
	return ld.names, ld.err
}

// fetch lists the instance repositories once and normalizes them to sorted
// "namespace/path" names (the same value the worker stores in the payload).
func (r *repoIndex) fetch(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	repos, err := r.lister.ListRepositories(ctx)
	if err != nil {
		return nil, fmt.Errorf("repo index: list repositories: %w", err)
	}
	names := make([]string, 0, len(repos))
	for _, p := range repos {
		name := p.Path
		if p.Namespace != "" {
			name = p.Namespace + "/" + p.Path
		}
		if name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// matchRepo matches raw as an exact name or as a suffix after dropping up to
// repoIndexMaxDrop leading segments of the stored name. The first level that
// yields matches wins (exact beats a shorter suffix).
func matchRepo(names []string, raw string) []string {
	for drop := 0; drop <= repoIndexMaxDrop; drop++ {
		var out []string
		for _, name := range names {
			segs := strings.Split(name, "/")
			if len(segs) <= drop {
				continue
			}
			if strings.Join(segs[drop:], "/") == raw {
				out = append(out, name)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

// clampCandidates trims an ambiguous resolution to the OR cap (T133).
func clampCandidates(candidates []string) []string {
	if len(candidates) > repoMaxCandidates {
		return candidates[:repoMaxCandidates]
	}
	return candidates
}
