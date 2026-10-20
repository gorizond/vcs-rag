// Package fake is a configurable provider.VCSProvider fake for tests.
package fake

import (
	"context"
	"strings"
	"time"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// Provider implements provider.VCSProvider over preconfigured data.
// Map keys are Repo.ID (data) and Job.ID (logs). Err* fields inject errors
// (isolation tests, US5). Methods honor context cancellation.
type Provider struct {
	Repos          []provider.Repo
	Issues         map[string][]provider.Issue
	ChangeRequests map[string][]provider.ChangeRequest
	Pipelines      map[string][]provider.Pipeline
	Logs           map[string]string // Job.ID → log text

	ErrListRepositories   error
	ErrListIssues         error
	ErrListChangeRequests error
	ErrListPipelines      error
	ErrPipelineLogTail    error

	// Optional provider capabilities (FR-015/FR-003): the account usage read
	// from RateLimit-* headers and the served page count. AccountUsageOK=false
	// models a response without headers.
	AccountUsage   float64
	AccountUsageOK bool
	PageCount      int64
	// US4/FR-008: observed 429 responses and the last Retry-After.
	RateLimitEventCount int64
	LastRetry           time.Duration
}

// RateUsage implements provider.RateLimitObserver (FR-015).
func (p *Provider) RateUsage() (float64, bool) { return p.AccountUsage, p.AccountUsageOK }

// Pages implements provider.PageCounter (FR-003).
func (p *Provider) Pages() int64 { return p.PageCount }

// RateLimitEvents implements provider.RateLimitEventCounter (FR-008).
func (p *Provider) RateLimitEvents() int64 { return p.RateLimitEventCount }

// LastRetryAfter implements provider.RetryAfterReporter (FR-008).
func (p *Provider) LastRetryAfter() time.Duration { return p.LastRetry }

// Type returns "fake".
func (p *Provider) Type() string { return "fake" }

// ListRepositories returns the configured repositories.
func (p *Provider) ListRepositories(ctx context.Context) ([]provider.Repo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.ErrListRepositories != nil {
		return nil, p.ErrListRepositories
	}
	return p.Repos, nil
}

// ListIssues returns the repository issues.
func (p *Provider) ListIssues(ctx context.Context, repo provider.Repo) ([]provider.Issue, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.ErrListIssues != nil {
		return nil, p.ErrListIssues
	}
	return p.Issues[repo.ID], nil
}

// ListChangeRequests returns the repository MR/PRs.
func (p *Provider) ListChangeRequests(ctx context.Context, repo provider.Repo) ([]provider.ChangeRequest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.ErrListChangeRequests != nil {
		return nil, p.ErrListChangeRequests
	}
	return p.ChangeRequests[repo.ID], nil
}

// ListPipelines returns the repository pipelines.
func (p *Provider) ListPipelines(ctx context.Context, repo provider.Repo) ([]provider.Pipeline, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.ErrListPipelines != nil {
		return nil, p.ErrListPipelines
	}
	return p.Pipelines[repo.ID], nil
}

// PipelineLogTail returns the job log tail — at most lines lines.
func (p *Provider) PipelineLogTail(ctx context.Context, repo provider.Repo, pipeline provider.Pipeline, job provider.Job, lines int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if p.ErrPipelineLogTail != nil {
		return "", p.ErrPipelineLogTail
	}
	logText := p.Logs[job.ID]
	if logText == "" || lines <= 0 {
		return "", nil
	}
	parts := strings.Split(strings.TrimRight(logText, "\n"), "\n")
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return strings.Join(parts, "\n"), nil
}
