// GitLab CI runners (T035/US4): the project's runner inventory.
package gitlab

import (
	"context"
	"fmt"

	gitlab "gitlab.com/gitlab-org/api/client-go/v3"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// ListRunners returns the runners available to the project
// (GET /projects/:id/runners, all pages — project and shared runners).
// Conditional domain: the endpoint needs Maintainer+ (Owner/Security
// Manager/Maintainer); a 403/404 is provider.ErrDomainUnavailable, so the
// graph lane turns the runner domain off instead of failing the pass.
//
// The list object carries no tag_list (client-go models tags only on
// RunnerDetails), so Tags stays empty — identity/state/type are complete;
// fetch GetRunnerDetails per runner only if tags ever matter.
func (c *Client) ListRunners(ctx context.Context, repo provider.Repo) ([]provider.Runner, error) {
	pid := repoFullPath(repo)
	opt := &gitlab.ListProjectRunnersOptions{ListOptions: gitlab.ListOptions{PerPage: 100}}
	var out []provider.Runner
	for {
		runners, resp, err := c.api.Runners.ListProjectRunners(pid, opt, gitlab.WithContext(ctx))
		if err != nil {
			if isDomainUnavailable(err) {
				return nil, provider.ErrDomainUnavailable
			}
			return nil, fmt.Errorf("gitlab: runners %s: %w", pid, err)
		}
		for _, r := range runners {
			state := r.Status
			if state == "" {
				state = "offline"
				if r.Online {
					state = "online"
				}
			}
			desc := r.Description
			if desc == "" {
				desc = r.Name
			}
			out = append(out, provider.Runner{
				ID:          int(r.ID),
				Description: desc,
				State:       state,
				RunnerType:  r.RunnerType,
				Online:      r.Online,
			})
		}
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return out, nil
}
