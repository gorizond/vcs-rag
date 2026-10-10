package mcp

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// fakeRepoLister is a controllable repoLister: counts calls, can block one
// fetch (single-flight) and inject errors (stale-on-error).
type fakeRepoLister struct {
	mu    sync.Mutex
	calls int
	repos []provider.Repo
	err   error
	gate  chan struct{} // non-nil: the fetch waits until it is closed
	slow  time.Duration // per-call delay (timeout test)
}

func (f *fakeRepoLister) ListRepositories(ctx context.Context) ([]provider.Repo, error) {
	f.mu.Lock()
	f.calls++
	gate, slow, err, repos := f.gate, f.slow, f.err, f.repos
	f.mu.Unlock()
	if gate != nil {
		<-gate
	}
	if slow > 0 {
		select {
		case <-time.After(slow):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	return repos, nil
}

func (f *fakeRepoLister) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeRepoLister) set(err error, repos []provider.Repo) {
	f.mu.Lock()
	f.err, f.repos = err, repos
	f.mu.Unlock()
}

// mkRepo splits a full name into namespace/path (the last segment is the path).
func mkRepo(full string) provider.Repo {
	i := strings.LastIndex(full, "/")
	return provider.Repo{ID: full, Namespace: full[:i], Path: full[i+1:]}
}

func mkRepos(names ...string) []provider.Repo {
	out := make([]provider.Repo, 0, len(names))
	for _, n := range names {
		out = append(out, mkRepo(n))
	}
	return out
}

// T132: exact match, suffix with ≤2 dropped leading segments, ambiguity and
// not-found.
func TestRepoIndexResolve(t *testing.T) {
	lister := &fakeRepoLister{repos: mkRepos(
		"gorizond/devops/nats", "gorizond/devops/api", "gorizond/secrets/runner")}
	idx := newRepoIndex(lister)

	check := func(raw string, want ...string) {
		t.Helper()
		got, err := idx.resolve(context.Background(), raw)
		if err != nil {
			t.Fatalf("resolve(%q): %v", raw, err)
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("resolve(%q) = %v, want %v", raw, got, want)
		}
	}

	check("gorizond/devops/nats", "gorizond/devops/nats") // exact
	check("secrets/runner", "gorizond/secrets/runner")    // drop 1 leading segment
	check("nats", "gorizond/devops/nats")                 // drop 2 leading segments
	check("api", "gorizond/devops/api")
	check("zzz") // not found
	check("")    // empty raw is a no-op

	// ambiguity: two matches at the same suffix level, sorted.
	amb := newRepoIndex(&fakeRepoLister{repos: mkRepos("a/x/nats", "b/y/nats")})
	got, err := amb.resolve(context.Background(), "nats")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "a/x/nats,b/y/nats" {
		t.Errorf("ambiguous = %v", got)
	}

	// the first level with matches wins: drop 1 beats drop 2.
	first := newRepoIndex(&fakeRepoLister{repos: mkRepos("a/runner", "b/c/runner")})
	got, err = first.resolve(context.Background(), "runner")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "a/runner" {
		t.Errorf("first-level = %v, want [a/runner]", got)
	}
}

// T132: TTL 60m ±10% — the list is cached and refetched only after expiry.
func TestRepoIndexTTL(t *testing.T) {
	lister := &fakeRepoLister{repos: mkRepos("g/one")}
	idx := newRepoIndex(lister)
	now := time.Unix(1000, 0)
	idx.now = func() time.Time { return now }
	idx.rnd = func() float64 { return 0.5 } // ttl factor = 1.0

	if _, err := idx.resolve(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(30 * time.Minute)
	if _, err := idx.resolve(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	if lister.count() != 1 {
		t.Fatalf("calls within ttl = %d, want 1", lister.count())
	}

	now = now.Add(31 * time.Minute)
	if _, err := idx.resolve(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	if lister.count() != 2 {
		t.Errorf("calls after ttl = %d, want 2", lister.count())
	}
}

// T132: one in-flight fetch is shared by concurrent resolvers (single-flight).
func TestRepoIndexSingleFlight(t *testing.T) {
	lister := &fakeRepoLister{repos: mkRepos("g/one"), gate: make(chan struct{})}
	idx := newRepoIndex(lister)

	results := make(chan []string, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			got, err := idx.resolve(context.Background(), "one")
			results <- got
			errs <- err
		}()
	}
	// the first resolver is blocked inside the fetch; the second must join it.
	deadline := time.Now().Add(2 * time.Second)
	for lister.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if n := lister.count(); n != 1 {
		t.Fatalf("in-flight calls = %d, want 1", n)
	}
	close(lister.gate)

	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		if got := <-results; len(got) != 1 || got[0] != "g/one" {
			t.Errorf("result = %v", got)
		}
	}
	if n := lister.count(); n != 1 {
		t.Errorf("total calls = %d, want 1", n)
	}
}

// T132: stale-on-error — a failing refetch keeps serving the last good list,
// and the retry is not attempted again within 5 minutes.
func TestRepoIndexStaleOnError(t *testing.T) {
	lister := &fakeRepoLister{repos: mkRepos("g/one")}
	idx := newRepoIndex(lister)
	now := time.Unix(1000, 0)
	idx.now = func() time.Time { return now }
	idx.rnd = func() float64 { return 0.5 }

	if _, err := idx.resolve(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	// expire and fail: the stale list is served, no error.
	now = now.Add(2 * time.Hour)
	lister.set(errors.New("provider down"), nil)
	got, err := idx.resolve(context.Background(), "one")
	if err != nil || len(got) != 1 || got[0] != "g/one" {
		t.Fatalf("stale-on-error = %v, %v", got, err)
	}
	if lister.count() != 2 {
		t.Fatalf("calls = %d, want 2", lister.count())
	}
	// within the retry cooldown there is no new fetch.
	now = now.Add(time.Minute)
	if _, err := idx.resolve(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	if lister.count() != 2 {
		t.Errorf("calls within cooldown = %d, want 2", lister.count())
	}
	// after the cooldown the fetch is retried and the new list is served.
	now = now.Add(6 * time.Minute)
	lister.set(nil, mkRepos("g/two"))
	got, err = idx.resolve(context.Background(), "two")
	if err != nil || len(got) != 1 || got[0] != "g/two" {
		t.Fatalf("after cooldown = %v, %v", got, err)
	}
	if lister.count() != 3 {
		t.Errorf("calls = %d, want 3", lister.count())
	}
}

// T132: an error without any cached list is returned, and the retry cooldown
// protects the provider from being hammered.
func TestRepoIndexErrorNoCache(t *testing.T) {
	lister := &fakeRepoLister{err: errors.New("boom")}
	idx := newRepoIndex(lister)
	now := time.Unix(1000, 0)
	idx.now = func() time.Time { return now }

	if _, err := idx.resolve(context.Background(), "one"); err == nil {
		t.Fatal("resolve with a failing lister expected an error")
	}
	if lister.count() != 1 {
		t.Fatalf("calls = %d, want 1", lister.count())
	}
	now = now.Add(time.Minute)
	if _, err := idx.resolve(context.Background(), "one"); err == nil {
		t.Fatal("resolve within cooldown expected an error")
	}
	if lister.count() != 1 {
		t.Errorf("calls within cooldown = %d, want 1 (no retry)", lister.count())
	}
	now = now.Add(6 * time.Minute)
	if _, err := idx.resolve(context.Background(), "one"); err == nil {
		t.Fatal("resolve after cooldown expected an error")
	}
	if lister.count() != 2 {
		t.Errorf("calls after cooldown = %d, want 2", lister.count())
	}
}

// T132: the provider fetch is bounded by the 20s timeout (shortened here).
func TestRepoIndexFetchTimeout(t *testing.T) {
	lister := &fakeRepoLister{repos: mkRepos("g/one"), slow: 500 * time.Millisecond}
	idx := newRepoIndex(lister)
	idx.timeout = 50 * time.Millisecond

	start := time.Now()
	if _, err := idx.resolve(context.Background(), "one"); err == nil {
		t.Fatal("resolve expected a timeout error")
	}
	if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
		t.Errorf("resolve took %v, want the 50ms timeout to cut the 500ms fetch", elapsed)
	}
}
