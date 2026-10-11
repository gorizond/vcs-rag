package github

import (
	"testing"
	"time"

	gh "github.com/google/go-github/v92/github"
)

// T030: the GitHub PR state mapping — merged/draft override the raw state;
// merged_at covers list responses without the merged flag.
func TestPRState(t *testing.T) {
	yes := true
	open, closed := "open", "closed"
	mergedAt := gh.Timestamp{Time: time.Unix(1, 0)}

	if got := prState(&gh.PullRequest{State: &open}); got != "open" {
		t.Errorf("open = %q", got)
	}
	if got := prState(&gh.PullRequest{State: &closed}); got != "closed" {
		t.Errorf("closed = %q", got)
	}
	if got := prState(&gh.PullRequest{State: &open, Draft: &yes}); got != "draft" {
		t.Errorf("draft = %q", got)
	}
	if got := prState(&gh.PullRequest{State: &closed, Merged: &yes}); got != "merged" {
		t.Errorf("merged = %q", got)
	}
	if got := prState(&gh.PullRequest{State: &open, MergedAt: &mergedAt}); got != "merged" {
		t.Errorf("merged_at = %q", got)
	}
}
