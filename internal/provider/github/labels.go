// GitHub labels (T029/US3): the repository label list behind the labelled
// edges.
package github

import (
	"context"
	"fmt"

	gh "github.com/google/go-github/v92/github"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// ListLabels returns the repository labels (all pages); a 403/404 is reported
// as provider.ErrDomainUnavailable. GitHub has no scoped labels, so Scope
// stays empty.
func (c *Client) ListLabels(ctx context.Context, repo provider.Repo) ([]provider.Label, error) {
	opt := &gh.ListOptions{PerPage: 100}
	var out []provider.Label
	for {
		labels, resp, err := c.api.Issues.ListLabels(ctx, repo.Namespace, repo.Path, opt)
		if err != nil {
			if domainUnavailable(err) {
				return nil, provider.ErrDomainUnavailable
			}
			return nil, fmt.Errorf("github: labels %s: %w", repo.Path, err)
		}
		for _, l := range labels {
			out = append(out, provider.Label{Name: l.GetName(), Color: l.GetColor()})
		}
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return out, nil
}
