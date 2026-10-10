package worker

import (
	"strings"
	"sync"
)

// repoLocks serializes work on a single repository (T110) without blocking the
// others. Locks are taken at the call sites only — the mirror/index/metadata
// lanes and the refresh composition — never inside ProcessRepoOnce/
// ProcessMetadataOnce/EnsureMirror: the refresh holds the lock for the whole
// composition and an inner lock would self-deadlock.
var (
	repoLocksMu sync.Mutex
	repoLocks   = make(map[string]*sync.Mutex)
)

// withRepoLock runs fn under the per-repository lock (slug = repoSlug).
func withRepoLock(slug string, fn func() error) error {
	repoLocksMu.Lock()
	mu, ok := repoLocks[slug]
	if !ok {
		mu = &sync.Mutex{}
		repoLocks[slug] = mu
	}
	repoLocksMu.Unlock()

	mu.Lock()
	defer mu.Unlock()
	return fn()
}

// repoSlug normalizes a repository identifier "namespace/path" into the
// refresh_status KV key form: every "/" replaced with "_" (T109/T112
// contract; the MCP side repeats this one-liner).
func repoSlug(id string) string {
	return strings.ReplaceAll(strings.Trim(strings.TrimSpace(id), "/"), "/", "_")
}
