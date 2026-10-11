// GitLab repository tree and branches (T032/US3): the structure family's
// directory/file and branch nodes.
package gitlab

import (
	"context"
	"fmt"

	gitlab "gitlab.com/gitlab-org/api/client-go/v3"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// ListTree returns the repository tree of the default branch, recursively (all
// pages): every blob and tree entry the structure nodes need. A 403/404 is
// reported as provider.ErrDomainUnavailable.
func (c *Client) ListTree(ctx context.Context, repo provider.Repo) ([]provider.TreeEntry, error) {
	pid := repoFullPath(repo)
	opt := &gitlab.ListTreeOptions{
		ListOptions: gitlab.ListOptions{PerPage: 100},
		Recursive:   gitlab.Ptr(true),
	}
	var out []provider.TreeEntry
	for {
		nodes, resp, err := c.api.Repositories.ListTree(pid, opt, gitlab.WithContext(ctx))
		if err != nil {
			if domainUnavailable(err) {
				return nil, provider.ErrDomainUnavailable
			}
			return nil, fmt.Errorf("gitlab: tree %s: %w", pid, err)
		}
		for _, n := range nodes {
			out = append(out, provider.TreeEntry{Path: n.Path, Name: n.Name, Type: n.Type})
		}
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return out, nil
}

// ListBranches returns the project branches (all pages); a 403/404 is reported
// as provider.ErrDomainUnavailable.
func (c *Client) ListBranches(ctx context.Context, repo provider.Repo) ([]provider.Branch, error) {
	pid := repoFullPath(repo)
	opt := &gitlab.ListBranchesOptions{ListOptions: gitlab.ListOptions{PerPage: 100}}
	var out []provider.Branch
	for {
		branches, resp, err := c.api.Branches.ListBranches(pid, opt, gitlab.WithContext(ctx))
		if err != nil {
			if domainUnavailable(err) {
				return nil, provider.ErrDomainUnavailable
			}
			return nil, fmt.Errorf("gitlab: branches %s: %w", pid, err)
		}
		for _, b := range branches {
			out = append(out, provider.Branch{Name: b.Name, Default: b.Default, Protected: b.Protected})
		}
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return out, nil
}
