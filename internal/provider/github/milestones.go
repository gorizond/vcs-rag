// GitHub milestones (T029/US3): the repository milestone list behind the
// in_milestone edges.
package github

import (
	"context"
	"fmt"

	gh "github.com/google/go-github/v92/github"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// ListMilestones returns the repository milestones (all pages, all states); a
// 403/404 is reported as provider.ErrDomainUnavailable. ID is the repo-local
// milestone number — the id ListWorkItems reports on an issue, so both sides
// of an in_milestone edge key on the same value.
func (c *Client) ListMilestones(ctx context.Context, repo provider.Repo) ([]provider.Milestone, error) {
	opt := &gh.MilestoneListOptions{State: "all", ListOptions: gh.ListOptions{PerPage: 100}}
	var out []provider.Milestone
	for {
		mss, resp, err := c.api.Issues.ListMilestones(ctx, repo.Namespace, repo.Path, opt)
		if err != nil {
			if domainUnavailable(err) {
				return nil, provider.ErrDomainUnavailable
			}
			return nil, fmt.Errorf("github: milestones %s: %w", repo.Path, err)
		}
		for _, m := range mss {
			ms := provider.Milestone{ID: m.GetNumber(), Title: m.GetTitle(), State: m.GetState()}
			if due := m.GetDueOn().Time; !due.IsZero() {
				ms.DueDate = &due
			}
			out = append(out, ms)
		}
		if resp.NextPage == 0 {
			break
		}
		opt.ListOptions.Page = resp.NextPage
	}
	return out, nil
}
