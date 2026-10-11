// GitLab metadata: issues, MRs, pipelines and log tails (US4/FR-012).
package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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

// milestoneTitle — the milestone title or "" (nil-safe).
func milestoneTitle(m *gitlab.Milestone) string {
	if m == nil {
		return ""
	}
	return m.Title
}

// issueUsernames — the logins of issue assignees (nil-safe).
func issueUsernames(users []*gitlab.IssueAssignee) []string {
	out := make([]string, 0, len(users))
	for _, u := range users {
		if u != nil {
			out = append(out, u.Username)
		}
	}
	return out
}

// usernames — the logins of MR assignees/reviewers (nil-safe).
func usernames(users []*gitlab.BasicUser) []string {
	out := make([]string, 0, len(users))
	for _, u := range users {
		if u != nil {
			out = append(out, u.Username)
		}
	}
	return out
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
			var author string
			if iss.Author != nil {
				author = iss.Author.Username
			}
			out = append(out, provider.Issue{
				ID:        iss.IID,
				Title:     iss.Title,
				Body:      iss.Description,
				State:     provider.NormalizeState(iss.State),
				URL:       iss.WebURL,
				UpdatedAt: timeOrZero(iss.UpdatedAt),
				Author:    author,
				Assignees: issueUsernames(iss.Assignees),
				Labels:    []string(iss.Labels),
				Milestone: milestoneTitle(iss.Milestone),
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
			var author string
			if mr.Author != nil {
				author = mr.Author.Username
			}
			out = append(out, provider.ChangeRequest{
				ID:           mr.IID,
				Title:        mr.Title,
				Body:         mr.Description,
				State:        provider.NormalizeState(mr.State),
				SourceBranch: mr.SourceBranch,
				TargetBranch: mr.TargetBranch,
				URL:          mr.WebURL,
				UpdatedAt:    timeOrZero(mr.UpdatedAt),
				Author:       author,
				Assignees:    usernames(mr.Assignees),
				Reviewers:    usernames(mr.Reviewers),
				Labels:       []string(mr.Labels),
				Milestone:    milestoneTitle(mr.Milestone),
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

// ListPipelineJobs returns every job of one pipeline with its stage context
// (T041/F4): stage sits on the job itself, duration is in fractional seconds,
// and successful as well as failed jobs are returned — the metadata lane keeps
// its own failed-only list, this reader is the graph lane's seam.
func (c *Client) ListPipelineJobs(ctx context.Context, repo provider.Repo, pipelineID int) ([]provider.CIPipelineDetail, error) {
	pid := repoFullPath(repo)
	opt := &gitlab.ListJobsOptions{ListOptions: gitlab.ListOptions{PerPage: 100}}
	var out []provider.CIPipelineDetail
	for {
		jobs, resp, err := c.api.Jobs.ListPipelineJobs(pid, int64(pipelineID), opt, gitlab.WithContext(ctx))
		if err != nil {
			return nil, fmt.Errorf("gitlab: pipeline jobs %s#%d: %w", pid, pipelineID, err)
		}
		for _, j := range jobs {
			sha := ""
			if j.Commit != nil {
				sha = j.Commit.ID
			}
			out = append(out, provider.CIPipelineDetail{
				PipelineID: pipelineID,
				Stage:      j.Stage,
				JobName:    j.Name,
				Status:     provider.NormalizeCIStatus(j.Status, ""),
				Ref:        j.Ref,
				CommitSHA:  sha,
				DurationMS: int64(j.Duration * 1000),
			})
		}
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return out, nil
}

// ListDeployments returns the project deployments (T041/F4), newest first, with
// the environment name and the normalized deployment status.
func (c *Client) ListDeployments(ctx context.Context, repo provider.Repo) ([]provider.DeploymentInfo, error) {
	pid := repoFullPath(repo)
	opt := &gitlab.ListProjectDeploymentsOptions{ListOptions: gitlab.ListOptions{PerPage: 100}}
	var out []provider.DeploymentInfo
	for {
		deps, resp, err := c.api.Deployments.ListProjectDeployments(pid, opt, gitlab.WithContext(ctx))
		if err != nil {
			return nil, fmt.Errorf("gitlab: deployments %s: %w", pid, err)
		}
		for _, d := range deps {
			env := ""
			if d.Environment != nil {
				env = d.Environment.Name
			}
			out = append(out, provider.DeploymentInfo{
				ID:          int(d.ID),
				Environment: env,
				Status:      provider.NormalizeCIStatus(d.Status, ""),
				Ref:         d.Ref,
				CommitSHA:   d.SHA,
			})
		}
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return out, nil
}

// ListEnvironments returns the project environments (T041/F4).
func (c *Client) ListEnvironments(ctx context.Context, repo provider.Repo) ([]provider.EnvironmentInfo, error) {
	pid := repoFullPath(repo)
	opt := &gitlab.ListEnvironmentsOptions{ListOptions: gitlab.ListOptions{PerPage: 100}}
	var out []provider.EnvironmentInfo
	for {
		envs, resp, err := c.api.Environments.ListEnvironments(pid, opt, gitlab.WithContext(ctx))
		if err != nil {
			return nil, fmt.Errorf("gitlab: environments %s: %w", pid, err)
		}
		for _, e := range envs {
			out = append(out, provider.EnvironmentInfo{
				Name:        e.Name,
				State:       e.State,
				Tier:        e.Tier,
				ExternalURL: e.ExternalURL,
			})
		}
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return out, nil
}

// ListVulnerabilities returns the project vulnerability findings (T046/F5).
// The endpoint is Ultimate-only: a 403/404 (missing tier or permission) is
// reported as provider.ErrVulnerabilitiesUnavailable instead of an error, so
// the graph lane turns the security domain off instead of failing the pass.
func (c *Client) ListVulnerabilities(ctx context.Context, repo provider.Repo) ([]provider.VulnerabilityFinding, error) {
	pid := repoFullPath(repo)
	opt := &gitlab.ListProjectVulnerabilitiesOptions{ListOptions: gitlab.ListOptions{PerPage: 100}}
	var out []provider.VulnerabilityFinding
	for {
		vulns, resp, err := c.api.ProjectVulnerabilities.ListProjectVulnerabilities(pid, opt, gitlab.WithContext(ctx))
		if err != nil {
			var er *gitlab.ErrorResponse
			if errors.As(err, &er) && (er.HasStatusCode(http.StatusForbidden) || er.HasStatusCode(http.StatusNotFound)) {
				return nil, provider.ErrVulnerabilitiesUnavailable
			}
			return nil, fmt.Errorf("gitlab: vulnerabilities %s: %w", pid, err)
		}
		for _, v := range vulns {
			raw := ""
			if v.Finding != nil {
				raw = v.Finding.RawMetadata
			}
			out = append(out, provider.VulnerabilityFinding{
				ID:           strconv.FormatInt(v.ID, 10),
				Severity:     vulnSeverity(v.Severity),
				Scanner:      v.ReportType,
				State:        v.State,
				LocationFile: findingFile(raw),
			})
		}
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return out, nil
}

// vulnSeverity maps a GitLab vulnerability severity onto the canonical set
// (critical|high|medium|low|info); an unknown/absent value becomes info.
func vulnSeverity(s string) string {
	switch s {
	case "critical", "high", "medium", "low":
		return s
	default:
		return "info"
	}
}

// findingFile extracts location.file from the raw metadata JSON of a finding
// ("" — unknown): the typed vulnerability object carries no location field.
func findingFile(raw string) string {
	if raw == "" {
		return ""
	}
	var md struct {
		Location struct {
			File string `json:"file"`
		} `json:"location"`
	}
	if json.Unmarshal([]byte(raw), &md) != nil {
		return ""
	}
	return md.Location.File
}
