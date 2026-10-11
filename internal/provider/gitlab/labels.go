// GitLab labels (T028/US3): the project label list behind the labelled edges.
package gitlab

import (
	"context"
	"fmt"

	gitlab "gitlab.com/gitlab-org/api/client-go/v3"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// ListLabels returns the project labels (all pages); a 403/404 is reported as
// provider.ErrDomainUnavailable. is_project_label maps to the normalized
// scope: a label inherited from a group is "group", a project's own "project".
func (c *Client) ListLabels(ctx context.Context, repo provider.Repo) ([]provider.Label, error) {
	pid := repoFullPath(repo)
	opt := &gitlab.ListLabelsOptions{ListOptions: gitlab.ListOptions{PerPage: 100}}
	var out []provider.Label
	for {
		labels, resp, err := c.api.Labels.ListLabels(pid, opt, gitlab.WithContext(ctx))
		if err != nil {
			if domainUnavailable(err) {
				return nil, provider.ErrDomainUnavailable
			}
			return nil, fmt.Errorf("gitlab: labels %s: %w", pid, err)
		}
		for _, l := range labels {
			scope := "group"
			if l.IsProjectLabel {
				scope = "project"
			}
			out = append(out, provider.Label{Name: l.Name, Scope: scope, Color: l.Color})
		}
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return out, nil
}
