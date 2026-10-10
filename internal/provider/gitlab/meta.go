// GitLab metadata: issues, MRs, pipelines and log tails (US4/FR-012).
package gitlab

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"time"

	gitlab "gitlab.com/gitlab-org/api/client-go/v3"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// repoFullPath returns the full GitLab project path ("namespace/path").
func repoFullPath(repo provider.Repo) string {
	if repo.Namespace == "" {
		return repo.Path
	}
	return repo.Namespace + "/" + repo.Path
}

// timeOrZero dereferences *time.Time.
func timeOrZero(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// ListIssues returns the project issues (all pages).
func (c *Client) ListIssues(ctx context.Context, repo provider.Repo) ([]provider.Issue, error) {
	pid := repoFullPath(repo)
	opt := &gitlab.ListProjectIssuesOptions{ListOptions: gitlab.ListOptions{PerPage: 100}}
	var out []provider.Issue
	for {
		issues, resp, err := c.api.Issues.ListProjectIssues(pid, opt, gitlab.WithContext(ctx))
		if err != nil {
			return nil, fmt.Errorf("gitlab: issues %s: %w", pid, err)
		}
		for _, iss := range issues {
			out = append(out, provider.Issue{
				ID:        iss.IID,
				Title:     iss.Title,
				Body:      iss.Description,
				State:     iss.State,
				URL:       iss.WebURL,
				UpdatedAt: timeOrZero(iss.UpdatedAt),
			})
		}
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return out, nil
}

// ListChangeRequests returns the project MRs (all pages).
func (c *Client) ListChangeRequests(ctx context.Context, repo provider.Repo) ([]provider.ChangeRequest, error) {
	pid := repoFullPath(repo)
	opt := &gitlab.ListProjectMergeRequestsOptions{ListOptions: gitlab.ListOptions{PerPage: 100}}
	var out []provider.ChangeRequest
	for {
		mrs, resp, err := c.api.MergeRequests.ListProjectMergeRequests(pid, opt, gitlab.WithContext(ctx))
		if err != nil {
			return nil, fmt.Errorf("gitlab: merge requests %s: %w", pid, err)
		}
		for _, mr := range mrs {
			out = append(out, provider.ChangeRequest{
				ID:           mr.IID,
				Title:        mr.Title,
				Body:         mr.Description,
				State:        mr.State,
				SourceBranch: mr.SourceBranch,
				TargetBranch: mr.TargetBranch,
				URL:          mr.WebURL,
				UpdatedAt:    timeOrZero(mr.UpdatedAt),
			})
		}
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return out, nil
}

// ListPipelines returns the CI_WINDOW of recent pipelines; jobs are fetched
// only for failed pipelines (the metadata lane looks at those only).
func (c *Client) ListPipelines(ctx context.Context, repo provider.Repo) ([]provider.Pipeline, error) {
	pid := repoFullPath(repo)
	opt := &gitlab.ListProjectPipelinesOptions{ListOptions: gitlab.ListOptions{PerPage: int64(c.ciWindow)}}
	infos, _, err := c.api.Pipelines.ListProjectPipelines(pid, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("gitlab: pipelines %s: %w", pid, err)
	}
	out := make([]provider.Pipeline, 0, len(infos))
	for _, p := range infos {
		pl := provider.Pipeline{
			ID:        strconv.FormatInt(p.ID, 10),
			Status:    p.Status,
			Ref:       p.Ref,
			SHA:       p.SHA,
			UpdatedAt: timeOrZero(p.UpdatedAt),
		}
		if p.Status == "failed" {
			jobs, _, err := c.api.Jobs.ListPipelineJobs(pid, p.ID,
				&gitlab.ListJobsOptions{ListOptions: gitlab.ListOptions{PerPage: 100}},
				gitlab.WithContext(ctx))
			if err != nil {
				return nil, fmt.Errorf("gitlab: jobs pipeline %d: %w", p.ID, err)
			}
			for _, j := range jobs {
				pl.Jobs = append(pl.Jobs, provider.Job{
					ID:       strconv.FormatInt(j.ID, 10),
					Name:     j.Name,
					Status:   j.Status,
					Duration: time.Duration(j.Duration * float64(time.Second)),
				})
			}
		}
		out = append(out, pl)
	}
	return out, nil
}

// PipelineLogTail returns the last lines lines of the job trace
// (the full log is not kept in memory).
func (c *Client) PipelineLogTail(ctx context.Context, repo provider.Repo, pipeline provider.Pipeline, job provider.Job, lines int) (string, error) {
	jobID, err := strconv.ParseInt(job.ID, 10, 64)
	if err != nil {
		return "", fmt.Errorf("gitlab: job id %q: %w", job.ID, err)
	}
	reader, _, err := c.api.Jobs.GetTraceFile(repoFullPath(repo), jobID, gitlab.WithContext(ctx))
	if err != nil {
		return "", fmt.Errorf("gitlab: trace job %s: %w", job.ID, err)
	}
	logText, err := io.ReadAll(reader)
	if err != nil {
		return "", fmt.Errorf("gitlab: read trace job %s: %w", job.ID, err)
	}
	return provider.TailLines(string(logText), lines), nil
}
