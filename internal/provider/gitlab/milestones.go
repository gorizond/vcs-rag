// GitLab milestones (T028/US3): the project milestone list behind the
// in_milestone edges.
package gitlab

import (
	"context"
	"fmt"
	"time"

	gitlab "gitlab.com/gitlab-org/api/client-go/v3"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// isoDate — the *ISOTime as *time.Time (nil-safe): the milestone date is
// optional, and a nil date must not become a zero one.
func isoDate(t *gitlab.ISOTime) *time.Time {
	if t == nil {
		return nil
	}
	d := time.Time(*t)
	return &d
}

// ListMilestones returns the project milestones (all pages); a 403/404 is
// reported as provider.ErrDomainUnavailable.
func (c *Client) ListMilestones(ctx context.Context, repo provider.Repo) ([]provider.Milestone, error) {
	pid := repoFullPath(repo)
	opt := &gitlab.ListMilestonesOptions{ListOptions: gitlab.ListOptions{PerPage: 100}}
	var out []provider.Milestone
	for {
		mss, resp, err := c.api.Milestones.ListMilestones(pid, opt, gitlab.WithContext(ctx))
		if err != nil {
			if domainUnavailable(err) {
				return nil, provider.ErrDomainUnavailable
			}
			return nil, fmt.Errorf("gitlab: milestones %s: %w", pid, err)
		}
		for _, m := range mss {
			out = append(out, provider.Milestone{
				ID:      int(m.ID),
				Title:   m.Title,
				State:   m.State,
				DueDate: isoDate(m.DueDate),
			})
		}
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return out, nil
}
