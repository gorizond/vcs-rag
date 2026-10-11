// GitHub metadata: issues, PRs, Actions runs and log tails (US4/FR-012).
package github

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v92/github"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// logins — the logins of users (nil-safe).
func logins(users []*gh.User) []string {
	out := make([]string, 0, len(users))
	for _, u := range users {
		if u != nil {
			out = append(out, u.GetLogin())
		}
	}
	return out
}

// labelNames — the names of labels (nil-safe).
func labelNames(labels []*gh.Label) []string {
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		if l != nil {
			out = append(out, l.GetName())
		}
	}
	return out
}

// prState — the canonical state of a PR (data-model §7): merged/draft override
// the raw open|closed; merged_at covers list responses without the merged flag.
func prState(pr *gh.PullRequest) string {
	switch {
	case pr.GetMerged() || !pr.GetMergedAt().IsZero():
		return "merged"
	case pr.GetDraft():
		return "draft"
	default:
		return provider.NormalizeState(pr.GetState())
	}
}

// ListIssues returns the repository issues (all pages).
func (c *Client) ListIssues(ctx context.Context, repo provider.Repo) ([]provider.Issue, error) {
	opt := &gh.IssueListByRepoOptions{State: "all", ListOptions: gh.ListOptions{PerPage: 100}}
	var out []provider.Issue
	for {
		issues, resp, err := c.api.Issues.ListByRepo(ctx, repo.Namespace, repo.Path, opt)
		if err != nil {
			return nil, fmt.Errorf("github: issues %s: %w", repo.Path, err)
		}
		for _, iss := range issues {
			out = append(out, provider.Issue{
				ID:        int64(iss.GetNumber()),
				Title:     iss.GetTitle(),
				Body:      iss.GetBody(),
				State:     provider.NormalizeState(iss.GetState()),
				URL:       iss.GetHTMLURL(),
				UpdatedAt: iss.GetUpdatedAt().Time,
				Author:    iss.GetUser().GetLogin(),
				Assignees: logins(iss.Assignees),
				Labels:    labelNames(iss.Labels),
				Milestone: iss.GetMilestone().GetTitle(),
			})
		}
		if resp.NextPage == 0 {
			break
		}
		opt.ListOptions.Page = resp.NextPage
	}
	return out, nil
}

// ListChangeRequests returns the repository PRs (all pages).
func (c *Client) ListChangeRequests(ctx context.Context, repo provider.Repo) ([]provider.ChangeRequest, error) {
	opt := &gh.PullRequestListOptions{State: "all", ListOptions: gh.ListOptions{PerPage: 100}}
	var out []provider.ChangeRequest
	for {
		prs, resp, err := c.api.PullRequests.List(ctx, repo.Namespace, repo.Path, opt)
		if err != nil {
			return nil, fmt.Errorf("github: pull requests %s: %w", repo.Path, err)
		}
		for _, pr := range prs {
			out = append(out, provider.ChangeRequest{
				ID:           int64(pr.GetNumber()),
				Title:        pr.GetTitle(),
				Body:         pr.GetBody(),
				State:        prState(pr),
				SourceBranch: pr.GetHead().GetRef(),
				TargetBranch: pr.GetBase().GetRef(),
				URL:          pr.GetHTMLURL(),
				UpdatedAt:    pr.GetUpdatedAt().Time,
				Author:       pr.GetUser().GetLogin(),
				Assignees:    logins(pr.Assignees),
				Reviewers:    logins(pr.RequestedReviewers),
				Labels:       labelNames(pr.Labels),
				Milestone:    pr.GetMilestone().GetTitle(),
			})
		}
		if resp.NextPage == 0 {
			break
		}
		opt.ListOptions.Page = resp.NextPage
	}
	return out, nil
}

// ListPipelines returns the CI_WINDOW of recent workflow runs; jobs are
// fetched only for failed runs (the metadata lane looks at those only).
func (c *Client) ListPipelines(ctx context.Context, repo provider.Repo) ([]provider.Pipeline, error) {
	opt := &gh.ListWorkflowRunsOptions{ListOptions: gh.ListOptions{PerPage: c.ciWindow}}
	runs, _, err := c.api.Actions.ListRepositoryWorkflowRuns(ctx, repo.Namespace, repo.Path, opt)
	if err != nil {
		return nil, fmt.Errorf("github: workflow runs %s: %w", repo.Path, err)
	}
	var out []provider.Pipeline
	for _, run := range runs.GetWorkflowRuns() {
		pl := provider.Pipeline{
			ID:        strconv.FormatInt(run.GetID(), 10),
			Status:    run.GetStatus(),
			Ref:       run.GetHeadBranch(),
			SHA:       run.GetHeadSHA(),
			UpdatedAt: run.GetUpdatedAt().Time,
		}
		if run.GetConclusion() == "failure" {
			jobs, _, err := c.api.Actions.ListWorkflowJobs(ctx, repo.Namespace, repo.Path, run.GetID(),
				&gh.ListWorkflowJobsOptions{Filter: "latest", ListOptions: gh.ListOptions{PerPage: 100}})
			if err != nil {
				return nil, fmt.Errorf("github: jobs run %d: %w", run.GetID(), err)
			}
			for _, j := range jobs.GetJobs() {
				pl.Jobs = append(pl.Jobs, provider.Job{
					ID:         strconv.FormatInt(j.GetID(), 10),
					Name:       j.GetName(),
					Status:     j.GetStatus(),
					Conclusion: j.GetConclusion(),
					Duration:   jobDuration(j),
				})
			}
		}
		out = append(out, pl)
	}
	return out, nil
}

// jobDuration returns the job duration from started/completed.
func jobDuration(j *gh.WorkflowJob) time.Duration {
	start, done := j.GetStartedAt(), j.GetCompletedAt()
	if start.IsZero() || done.IsZero() {
		return 0
	}
	return done.Time.Sub(start.Time)
}

// PipelineLogTail downloads the run log archive and returns the last lines
// lines of the specified job log.
func (c *Client) PipelineLogTail(ctx context.Context, repo provider.Repo, pipeline provider.Pipeline, job provider.Job, lines int) (string, error) {
	runID, err := strconv.ParseInt(pipeline.ID, 10, 64)
	if err != nil {
		return "", fmt.Errorf("github: run id %q: %w", pipeline.ID, err)
	}
	logURL, _, err := c.api.Actions.GetWorkflowRunLogs(ctx, repo.Namespace, repo.Path, runID, 0)
	if err != nil {
		return "", fmt.Errorf("github: log link for run %s: %w", pipeline.ID, err)
	}
	data, err := c.downloadLogs(ctx, logURL.String())
	if err != nil {
		return "", err
	}
	text, err := extractJobLog(data, job.Name)
	if err != nil {
		return "", err
	}
	return provider.TailLines(text, lines), nil
}

// downloadLogs fetches the log zip from the signed redirect URL — without
// the Authorization header (the URL is pre-authorized), using the client timeout.
func (c *Client) downloadLogs(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.logsHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github: log download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github: log download: HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 256<<20))
}

// extractJobLog gathers the job steps from the Actions archive (directory =
// job name) and concatenates them in step-number order.
func extractJobLog(data []byte, jobName string) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("github: log zip: %w", err)
	}
	prefix := jobName + "/"
	var entries []*zip.File
	for _, f := range zr.File {
		if strings.HasPrefix(f.Name, prefix) {
			entries = append(entries, f)
		}
	}
	if len(entries) == 0 {
		return "", fmt.Errorf("github: job log %q not found in the archive", jobName)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })

	var sb strings.Builder
	for _, f := range entries {
		rc, err := f.Open()
		if err != nil {
			return "", fmt.Errorf("github: read step %s: %w", f.Name, err)
		}
		chunk, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			return "", fmt.Errorf("github: read step %s: %w", f.Name, err)
		}
		sb.Write(chunk)
		if !strings.HasSuffix(sb.String(), "\n") {
			sb.WriteByte('\n')
		}
	}
	return sb.String(), nil
}
