// GitHub metadata: issues, PRs, Actions runs and log tails (US4/FR-012).
package github

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
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

// ListPipelineJobs returns every job of one workflow run (T042/F4): the run
// carries ref/sha, each job its status/conclusion and started/completed
// timestamps. Actions has no stage concept, so Stage stays empty; the API also
// returns per-step details, but the graph model has no step level — only the
// job row is stored.
func (c *Client) ListPipelineJobs(ctx context.Context, repo provider.Repo, pipelineID int) ([]provider.CIPipelineDetail, error) {
	run, _, err := c.api.Actions.GetWorkflowRunByID(ctx, repo.Namespace, repo.Path, int64(pipelineID))
	if err != nil {
		return nil, fmt.Errorf("github: workflow run %s#%d: %w", repo.Path, pipelineID, err)
	}
	opt := &gh.ListWorkflowJobsOptions{Filter: "latest", ListOptions: gh.ListOptions{PerPage: 100}}
	var out []provider.CIPipelineDetail
	for {
		jobs, resp, err := c.api.Actions.ListWorkflowJobs(ctx, repo.Namespace, repo.Path, int64(pipelineID), opt)
		if err != nil {
			return nil, fmt.Errorf("github: jobs run %d: %w", pipelineID, err)
		}
		for _, j := range jobs.GetJobs() {
			out = append(out, provider.CIPipelineDetail{
				PipelineID: pipelineID,
				JobName:    j.GetName(),
				Status:     provider.NormalizeCIStatus(j.GetStatus(), j.GetConclusion()),
				Ref:        run.GetHeadBranch(),
				CommitSHA:  run.GetHeadSHA(),
				DurationMS: jobDuration(j).Milliseconds(),
			})
		}
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return out, nil
}

// ListDeployments returns the recent deployments (T042/F4) with the state of
// the latest deployment status. ponytail: one page (100) and one status call
// per deployment — an N+1; paginate wider only if deployments ever matter.
func (c *Client) ListDeployments(ctx context.Context, repo provider.Repo) ([]provider.DeploymentInfo, error) {
	deps, _, err := c.api.Repositories.ListDeployments(ctx, repo.Namespace, repo.Path,
		&gh.DeploymentsListOptions{ListOptions: gh.ListOptions{PerPage: 100}})
	if err != nil {
		return nil, fmt.Errorf("github: deployments %s: %w", repo.Path, err)
	}
	out := make([]provider.DeploymentInfo, 0, len(deps))
	for _, d := range deps {
		status := ""
		states, _, err := c.api.Repositories.ListDeploymentStatuses(ctx, repo.Namespace, repo.Path,
			d.GetID(), &gh.ListOptions{PerPage: 1})
		if err != nil {
			return nil, fmt.Errorf("github: deployment statuses %s#%d: %w", repo.Path, d.GetID(), err)
		}
		if len(states) > 0 {
			status = provider.NormalizeCIStatus(states[0].GetState(), "")
		}
		out = append(out, provider.DeploymentInfo{
			ID:          int(d.GetID()),
			Environment: d.GetEnvironment(),
			Status:      status,
			Ref:         d.GetRef(),
			CommitSHA:   d.GetSHA(),
		})
	}
	return out, nil
}

// ListEnvironments returns the repository environments (T042/F4).
func (c *Client) ListEnvironments(ctx context.Context, repo provider.Repo) ([]provider.EnvironmentInfo, error) {
	opt := &gh.EnvironmentListOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	var out []provider.EnvironmentInfo
	for {
		envs, resp, err := c.api.Repositories.ListEnvironments(ctx, repo.Namespace, repo.Path, opt)
		if err != nil {
			return nil, fmt.Errorf("github: environments %s: %w", repo.Path, err)
		}
		for _, e := range envs.GetEnvironments() {
			out = append(out, provider.EnvironmentInfo{Name: e.GetName()})
		}
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return out, nil
}

// ListVulnerabilities returns the open Dependabot and code-scanning alerts
// (T046/F5). A 403/404 on both endpoints means the token lacks the
// security_events scope (or the feature is off) — reported as
// provider.ErrVulnerabilitiesUnavailable; when only one endpoint is off the
// other's findings are still returned.
func (c *Client) ListVulnerabilities(ctx context.Context, repo provider.Repo) ([]provider.VulnerabilityFinding, error) {
	var out []provider.VulnerabilityFinding
	depOff, csOff := false, false

	deps, _, err := c.api.Dependabot.ListRepoAlerts(ctx, repo.Namespace, repo.Path,
		&gh.ListAlertsOptions{State: gh.Ptr("open"), ListOptions: gh.ListOptions{PerPage: 100}})
	switch {
	case err == nil:
		for _, a := range deps {
			out = append(out, provider.VulnerabilityFinding{
				ID:           strconv.Itoa(a.GetNumber()),
				Severity:     depSeverity(a),
				Scanner:      "dependabot",
				State:        vulnState(a.GetState()),
				LocationFile: a.GetDependency().GetManifestPath(),
			})
		}
	case isSecurityUnavailable(err):
		depOff = true
	default:
		return nil, fmt.Errorf("github: dependabot alerts %s: %w", repo.Path, err)
	}

	alerts, _, err := c.api.CodeScanning.ListAlertsForRepo(ctx, repo.Namespace, repo.Path,
		&gh.AlertListOptions{State: "open", ListOptions: gh.ListOptions{PerPage: 100}})
	switch {
	case err == nil:
		for _, a := range alerts {
			out = append(out, provider.VulnerabilityFinding{
				ID:           strconv.Itoa(a.GetNumber()),
				Severity:     scanSeverity(a),
				Scanner:      "code-scanning",
				State:        vulnState(a.GetState()),
				LocationFile: a.GetMostRecentInstance().GetLocation().GetPath(),
			})
		}
	case isSecurityUnavailable(err):
		csOff = true
	default:
		return nil, fmt.Errorf("github: code scanning alerts %s: %w", repo.Path, err)
	}

	if depOff && csOff {
		return nil, provider.ErrVulnerabilitiesUnavailable
	}
	return out, nil
}

// isSecurityUnavailable reports whether err is a GitHub 403/404: the endpoint
// is disabled or the token lacks the security_events scope.
func isSecurityUnavailable(err error) bool {
	var er *gh.ErrorResponse
	return errors.As(err, &er) && er.Response != nil &&
		(er.Response.StatusCode == http.StatusForbidden || er.Response.StatusCode == http.StatusNotFound)
}

// depSeverity maps a Dependabot advisory severity onto the canonical set;
// "moderate" is the old GitHub spelling of "medium".
func depSeverity(a *gh.DependabotAlert) string {
	switch s := a.GetSecurityAdvisory().GetSeverity(); s {
	case "critical", "high", "low":
		return s
	case "moderate":
		return "medium"
	default:
		return "info"
	}
}

// scanSeverity maps a code-scanning alert onto the canonical severity: the
// security severity level wins, the plain rule severity is the fallback.
func scanSeverity(a *gh.Alert) string {
	switch s := a.GetRule().GetSecuritySeverityLevel(); s {
	case "critical", "high", "medium", "low":
		return s
	}
	switch a.GetRule().GetSeverity() {
	case "error":
		return "high"
	case "warning":
		return "medium"
	default:
		return "info"
	}
}

// vulnState maps a GitHub alert state (open|dismissed|fixed|auto_dismissed)
// onto the canonical vulnerability state (detected|dismissed|resolved).
func vulnState(state string) string {
	switch state {
	case "dismissed", "auto_dismissed":
		return "dismissed"
	case "fixed":
		return "resolved"
	default:
		return "detected"
	}
}
