// GitHub CI runners (T035/US4): the runner set derived from recent job runs.
package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	gh "github.com/google/go-github/v92/github"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// runnerJobWindow bounds the runs inspected for runner names: the active
// runner set changes slowly, so the most recent runs are enough to enumerate
// it.
// ponytail: raise the window if fleet churn makes recent runners disappear.
const runnerJobWindow = 10

// ListRunners returns the runners the repository's recent CI jobs ran on:
// GitHub has no repository runner inventory without repo admin
// (GET /repos/:o/:r/actions/runners is admin-only), so the reader derives the
// set from each job's runner_name — the available source. The runner has no
// numeric id in this view: the name is stored in Description and doubles as
// the node identity. A 403 (no Actions read access) is
// provider.ErrDomainUnavailable; a per-run 404 (deleted run) just skips that
// run.
func (c *Client) ListRunners(ctx context.Context, repo provider.Repo) ([]provider.Runner, error) {
	opt := &gh.ListWorkflowRunsOptions{ListOptions: gh.ListOptions{PerPage: c.ciWindow}}
	runs, _, err := c.api.Actions.ListRepositoryWorkflowRuns(ctx, repo.Namespace, repo.Path, opt)
	if err != nil {
		if isSecurityUnavailable(err) {
			return nil, provider.ErrDomainUnavailable
		}
		return nil, fmt.Errorf("github: workflow runs %s: %w", repo.Path, err)
	}
	seen := map[string]bool{}
	var out []provider.Runner
	for i, run := range runs.GetWorkflowRuns() {
		if i >= runnerJobWindow || ctx.Err() != nil {
			break
		}
		jobs, _, err := c.api.Actions.ListWorkflowJobs(ctx, repo.Namespace, repo.Path, run.GetID(),
			&gh.ListWorkflowJobsOptions{Filter: "latest", ListOptions: gh.ListOptions{PerPage: 100}})
		if err != nil {
			var er *gh.ErrorResponse
			if errors.As(err, &er) && er.Response != nil && er.Response.StatusCode == http.StatusForbidden {
				return nil, provider.ErrDomainUnavailable
			}
			continue // a deleted/expired run: its jobs are not enumerable
		}
		for _, j := range jobs.GetJobs() {
			name := j.GetRunnerName()
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, provider.Runner{Description: name})
		}
	}
	return out, nil
}
