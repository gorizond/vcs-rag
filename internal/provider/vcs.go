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

// ErrVulnerabilitiesUnavailable marks the security domain as unavailable on
// this provider/instance (T046/F5): a 403/404 or a token missing the security
// scope is a capability signal, not a failure — the caller skips the domain
// instead of failing the pass.
var ErrVulnerabilitiesUnavailable = errors.New("vulnerabilities unavailable")

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

// NormalizeState maps a raw provider state onto the canonical vocabulary
// open|merged|closed|locked|draft (data-model §7, F3): GitLab says "opened",
// GitHub's merged/draft are derived by its provider (needs PR flags).
func NormalizeState(state string) string {
	if state == "opened" {
		return "open"
	}
	return state
}

// NormalizeCIStatus maps a raw CI state onto the canonical vocabulary
// success|failed|running|pending|canceled|skipped (data-model §5, F4). GitLab
// passes the job/pipeline status (conclusion empty); GitHub passes the
// workflow/job (status, conclusion) pair.
func NormalizeCIStatus(status, conclusion string) string {
	switch conclusion {
	case "success":
		return "success"
	case "failure", "timed_out":
		return "failed"
	case "cancelled":
		return "canceled"
	case "skipped", "neutral":
		return "skipped"
	}
	switch status {
	case "success":
		return "success"
	case "failed":
		return "failed"
	case "running", "in_progress":
		return "running"
	case "canceled", "cancelled", "cancelling":
		return "canceled"
	case "skipped":
		return "skipped"
	}
	return "pending"
}

// Issue is an issue/task (GitLab issue = GitHub issue).
type Issue struct {
	ID        int64
	Title     string
	Body      string
	State     string
	URL       string
	UpdatedAt time.Time
	// F3 people/planning fields (additive; data-model §7).
	Author    string
	Assignees []string
	Labels    []string
	Milestone string
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
	// F3 people/planning fields (additive; data-model §7).
	Author    string
	Assignees []string
	Reviewers []string
	Labels    []string
	Milestone string
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

// ChangeRequestFile is a single file diff of a change request (R12).
type ChangeRequestFile struct {
	OldPath   string
	NewPath   string
	Status    string // added | modified | deleted | renamed
	Patch     string
	Truncated bool
	MRID      int
}

// CIPipelineDetail is one job of a CI pipeline with its stage context (R12).
type CIPipelineDetail struct {
	PipelineID int
	Stage      string
	JobName    string
	Status     string
	Ref        string
	CommitSHA  string
	Env        string
	DurationMS int64
}

// DeploymentInfo is a normalized deployment record (R12).
type DeploymentInfo struct {
	ID          int
	Environment string
	Status      string
	Ref         string
	CommitSHA   string
}

// EnvironmentInfo is a normalized CI/CD environment (R12).
type EnvironmentInfo struct {
	Name        string
	State       string
	Tier        string
	ExternalURL string
}

// VulnerabilityFinding is a normalized security finding (R12).
type VulnerabilityFinding struct {
	ID           string
	Severity     string
	Scanner      string
	State        string
	LocationFile string
}

// Optional capability interfaces (R12). Implementations may satisfy any
// subset; callers type-assert to detect support.

// ChangeRequestDiffProvider lists file diffs of a change request.
type ChangeRequestDiffProvider interface {
	ListChangeRequestDiffs(ctx context.Context, repo Repo, mrID int) ([]ChangeRequestFile, error)
}

// CIPipelineDetailProvider exposes pipeline jobs, deployments and environments.
type CIPipelineDetailProvider interface {
	ListPipelineJobs(ctx context.Context, repo Repo, pipelineID int) ([]CIPipelineDetail, error)
	ListDeployments(ctx context.Context, repo Repo) ([]DeploymentInfo, error)
	ListEnvironments(ctx context.Context, repo Repo) ([]EnvironmentInfo, error)
}

// VulnerabilityProvider lists security findings of a repository.
type VulnerabilityProvider interface {
	ListVulnerabilities(ctx context.Context, repo Repo) ([]VulnerabilityFinding, error)
}
