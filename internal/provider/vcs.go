// Package provider defines a VCS-agnostic interface to GitLab/GitHub
// and normalized metadata types.
package provider

import (
	"context"
	"errors"
	"strings"
	"time"
)

// ErrNotImplemented is returned by method stubs; implemented in US4.
var ErrNotImplemented = errors.New("not implemented")

// TailLines returns the last n lines of the text (n<=0 — no truncation).
func TailLines(s string, n int) string {
	if n <= 0 || s == "" {
		return s
	}
	s = strings.TrimRight(s, "\n")
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

// VCSProvider is a source of repositories and metadata (FR-003/004).
type VCSProvider interface {
	Type() string // "gitlab" | "github"
	ListRepositories(ctx context.Context) ([]Repo, error)
	ListIssues(ctx context.Context, repo Repo) ([]Issue, error)
	ListChangeRequests(ctx context.Context, repo Repo) ([]ChangeRequest, error)
	ListPipelines(ctx context.Context, repo Repo) ([]Pipeline, error)
	PipelineLogTail(ctx context.Context, repo Repo, pipeline Pipeline, job Job, lines int) (string, error)
}

// Repo is a normalized repository.
type Repo struct {
	ID            string
	Namespace     string
	Path          string
	CloneURL      string
	DefaultBranch string
}

// Issue is an issue/task (GitLab issue = GitHub issue).
type Issue struct {
	ID        int64
	Title     string
	Body      string
	State     string
	URL       string
	UpdatedAt time.Time
}

// ChangeRequest — MR (GitLab) / PR (GitHub).
type ChangeRequest struct {
	ID           int64
	Title        string
	Body         string
	State        string
	SourceBranch string
	TargetBranch string
	URL          string
	UpdatedAt    time.Time
}

// Pipeline is a CI pipeline with jobs.
type Pipeline struct {
	ID        string
	Status    string
	Ref       string
	SHA       string
	UpdatedAt time.Time
	Jobs      []Job
}

// Job is a pipeline job.
type Job struct {
	ID         string
	Name       string
	Status     string
	Conclusion string
	Duration   time.Duration
}

// RateLimit is the API rate limit state.
type RateLimit struct {
	Remaining  int
	Reset      time.Time
	RetryAfter time.Duration
}
