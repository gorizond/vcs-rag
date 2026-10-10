package worker

import (
	"testing"
	"time"
)

// T110: one repository — serialized; a different repository — not blocked.
func TestWithRepoLockSerializesOneRepo(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		_ = withRepoLock("a/b", func() error { close(entered); <-release; return nil })
		close(done)
	}()
	<-entered

	second := make(chan struct{})
	go func() { _ = withRepoLock("a/b", func() error { close(second); return nil }) }()
	select {
	case <-second:
		t.Fatal("the second holder entered while the first was running")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	<-done
	select {
	case <-second:
	case <-time.After(time.Second):
		t.Fatal("the second holder did not run after the release")
	}
}

func TestWithRepoLockDifferentReposParallel(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	go func() { _ = withRepoLock("a/x", func() error { close(entered); <-release; return nil }) }()
	<-entered

	other := make(chan struct{})
	go func() { _ = withRepoLock("a/y", func() error { close(other); return nil }) }()
	select {
	case <-other:
	case <-time.After(time.Second):
		t.Fatal("another repository was blocked by a different repo lock")
	}
	close(release)
}

func TestRepoSlug(t *testing.T) {
	for in, want := range map[string]string{
		"e2e/grp/fixture": "e2e_grp_fixture",
		"/a/b/":           "a_b",
		" plain/repo ":    "plain_repo",
		"repo":            "repo",
	} {
		if got := repoSlug(in); got != want {
			t.Errorf("repoSlug(%q) = %q, want %q", in, got, want)
		}
	}
}
