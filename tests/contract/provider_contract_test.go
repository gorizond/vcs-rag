// Package contract — reusable contract suite for provider.VCSProvider.
// Currently runs against the fake; real GitLab/GitHub providers are wired in later.
package contract

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gorizond/vcs-rag/internal/provider"
	"github.com/gorizond/vcs-rag/internal/provider/fake"
)

// RunProviderContract checks the common invariants of a VCSProvider implementation.
func RunProviderContract(t *testing.T, p provider.VCSProvider) {
	t.Helper()
	ctx := context.Background()

	if p.Type() == "" {
		t.Error("Type() is empty")
	}

	repos, err := p.ListRepositories(ctx)
	if err != nil {
		t.Fatalf("ListRepositories: %v", err)
	}
	if len(repos) == 0 {
		t.Fatal("ListRepositories: empty result")
	}
	seen := make(map[string]bool)
	for _, r := range repos {
		if r.ID == "" || r.Namespace == "" || r.Path == "" || r.CloneURL == "" {
			t.Errorf("repo %+v: empty required fields ID/Namespace/Path/CloneURL", r)
		}
		if seen[r.ID] {
			t.Errorf("repo ID %q is not unique", r.ID)
		}
		seen[r.ID] = true
	}

	repos2, err := p.ListRepositories(ctx)
	if err != nil {
		t.Fatalf("ListRepositories (repeat): %v", err)
	}
	if !reflect.DeepEqual(repos, repos2) {
		t.Error("ListRepositories is not deterministic")
	}

	repo := repos[0]

	issues1, err := p.ListIssues(ctx, repo)
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	issues2, err := p.ListIssues(ctx, repo)
	if err != nil {
		t.Fatalf("ListIssues (repeat): %v", err)
	}
	if !reflect.DeepEqual(issues1, issues2) {
		t.Error("ListIssues is not deterministic")
	}

	pipelines, err := p.ListPipelines(ctx, repo)
	if err != nil {
		t.Fatalf("ListPipelines: %v", err)
	}

	// Context cancellation → error (context.Canceled).
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := p.ListRepositories(cancelled); !errors.Is(err, context.Canceled) {
		t.Errorf("ListRepositories: context cancellation gave err=%v", err)
	}
	if _, err := p.ListIssues(cancelled, repo); !errors.Is(err, context.Canceled) {
		t.Errorf("ListIssues: context cancellation gave err=%v", err)
	}
	if _, err := p.ListChangeRequests(cancelled, repo); !errors.Is(err, context.Canceled) {
		t.Errorf("ListChangeRequests: context cancellation gave err=%v", err)
	}
	if _, err := p.ListPipelines(cancelled, repo); !errors.Is(err, context.Canceled) {
		t.Errorf("ListPipelines: context cancellation gave err=%v", err)
	}

	// PipelineLogTail: no more than N lines; context cancellation → error.
	if len(pipelines) > 0 && len(pipelines[0].Jobs) > 0 {
		const n = 3
		job := pipelines[0].Jobs[0]
		tail, err := p.PipelineLogTail(ctx, repo, pipelines[0], job, n)
		if err != nil {
			t.Fatalf("PipelineLogTail: %v", err)
		}
		if lines := countLines(tail); lines > n {
			t.Errorf("PipelineLogTail returned %d lines, expected at most %d", lines, n)
		}
		if _, err := p.PipelineLogTail(cancelled, repo, pipelines[0], job, n); !errors.Is(err, context.Canceled) {
			t.Errorf("PipelineLogTail: context cancellation gave err=%v", err)
		}
	}
}

// countLines counts lines, ignoring a single trailing newline.
func countLines(s string) int {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

// TestFakeProvider runs the contract against the fake.
func TestFakeProvider(t *testing.T) {
	f := &fake.Provider{
		Repos: []provider.Repo{
			{ID: "1", Namespace: "grp", Path: "grp/a", CloneURL: "https://example.com/grp/a.git", DefaultBranch: "main"},
			{ID: "2", Namespace: "grp", Path: "grp/b", CloneURL: "https://example.com/grp/b.git", DefaultBranch: "main"},
		},
		Issues: map[string][]provider.Issue{
			"1": {{ID: 10, Title: "issue", State: "opened"}},
		},
		Pipelines: map[string][]provider.Pipeline{
			"1": {{ID: "100", Status: "success", Ref: "main",
				Jobs: []provider.Job{{ID: "1000", Name: "build", Status: "success", Duration: 5 * time.Second}}}},
		},
		Logs: map[string]string{
			"1000": "l1\nl2\nl3\nl4\nl5\n",
		},
	}
	RunProviderContract(t, f)
}
