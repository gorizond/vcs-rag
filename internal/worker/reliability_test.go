package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v92/github"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// (a) a single repository failure does not stop the others: N−1 processed.
func TestProcessSafelyIsolation(t *testing.T) {
	repos := []provider.Repo{
		{ID: "1", Namespace: "g", Path: "a"},
		{ID: "2", Namespace: "g", Path: "b"},
		{ID: "3", Namespace: "g", Path: "c"},
	}
	var processed []string
	failures := 0
	for _, repo := range repos {
		err := processSafely(context.Background(), testLogger(), repo, "index", func() error {
			if repo.ID == "2" {
				return errors.New("boom")
			}
			processed = append(processed, repo.ID)
			return nil
		})
		if err != nil {
			failures++
			if repo.ID != "2" {
				t.Errorf("unexpected failure of repo %s: %v", repo.ID, err)
			}
		}
	}
	if len(processed) != len(repos)-1 {
		t.Errorf("processed %d, want N−1 = %d", len(processed), len(repos)-1)
	}
	if failures != 1 {
		t.Errorf("failures %d, want 1", failures)
	}
}

// a panic is isolated and returned as an error — the lane keeps running.
func TestProcessSafelyPanic(t *testing.T) {
	err := processSafely(context.Background(), testLogger(), provider.Repo{ID: "1"}, "index", func() error {
		panic("kaboom")
	})
	if err == nil {
		t.Fatal("panic must be returned as an error")
	}
	if !strings.Contains(err.Error(), "kaboom") {
		t.Errorf("err = %v", err)
	}
}

// (b) IsRateLimit: GitHub types, generic 429/Retry-After, errors.As unwrapping.
func TestIsRateLimit(t *testing.T) {
	retry := 30 * time.Second
	resetIn := 10 * time.Second
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"github abuse rate limit", &gh.AbuseRateLimitError{RetryAfter: &retry}, true},
		{"github rate limit", &gh.RateLimitError{Rate: gh.Rate{Reset: gh.Timestamp{Time: time.Now().Add(resetIn)}}}, true},
		{"generic 429", errors.New("HTTP 429: too many requests"), true},
		{"generic retry-after", errors.New("server said Retry-After: 3"), true},
		{"generic connection error", errors.New("connection refused"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := provider.IsRateLimit(tc.err); ok != tc.want {
				t.Errorf("IsRateLimit(%v) ok=%v, want %v", tc.err, ok, tc.want)
			}
		})
	}

	if d, ok := provider.IsRateLimit(&gh.AbuseRateLimitError{RetryAfter: &retry}); !ok || d != retry {
		t.Errorf("retryAfter = %v (ok=%v), want %v", d, ok, retry)
	}
	if d, ok := provider.IsRateLimit(&gh.RateLimitError{Rate: gh.Rate{Reset: gh.Timestamp{Time: time.Now().Add(resetIn)}}}); !ok || d <= 0 {
		t.Errorf("rate reset retryAfter = %v (ok=%v), want > 0", d, ok)
	}
	wrapped := fmt.Errorf("list repos: %w", &gh.AbuseRateLimitError{RetryAfter: &retry})
	if _, ok := provider.IsRateLimit(wrapped); !ok {
		t.Error("errors.As did not unwrap the wrapped error")
	}
}

// (c) resume: a repeated pass yields no commits to index
// (the full point counter is covered by e2e US2–US4 on a real Qdrant).
func TestNewCommitsResume(t *testing.T) {
	window := []string{"c", "b", "a"} // newest→oldest
	if got := newCommits(window, nil); len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Errorf("first pass = %v, want [a b c] (oldest→newest)", got)
	}
	if got := newCommits(window, []string{"a", "b", "c"}); len(got) != 0 {
		t.Errorf("repeated pass = %v, want empty (resume)", got)
	}
	if got := newCommits(window, []string{"a", "b"}); len(got) != 1 || got[0] != "c" {
		t.Errorf("increment = %v, want [c]", got)
	}
}

// T059: flock lock — the second AcquireLock is busy, free again after release.
func TestAcquireLock(t *testing.T) {
	dir := t.TempDir()
	release, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	_, err = AcquireLock(dir)
	if err == nil {
		t.Fatal("the second AcquireLock must return the busy error")
	}
	if !strings.Contains(err.Error(), "worker already running") {
		t.Errorf("err = %v", err)
	}
	release()

	release2, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("AcquireLock after release: %v", err)
	}
	release2()
}
