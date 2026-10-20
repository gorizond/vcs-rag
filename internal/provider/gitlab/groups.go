// GitLab groups and group projects (T038/US4): the core family's group and
// project nodes.
package gitlab

import (
	"context"
	"fmt"
	"strconv"

	gitlab "gitlab.com/gitlab-org/api/client-go/v3"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// ListGroups returns the groups visible to the token (all pages; subgroups
// included, since a group node carries parent_id). A 403/404 is reported as
// provider.ErrDomainUnavailable.
func (c *Client) ListGroups(ctx context.Context) ([]provider.Group, error) {
	opt := &gitlab.ListGroupsOptions{ListOptions: gitlab.ListOptions{PerPage: 100}}
	var out []provider.Group
	for {
		if err := waitRate(ctx, c.groupRate); err != nil {
			return nil, err
		}
		groups, resp, err := c.api.Groups.ListGroups(opt, gitlab.WithContext(ctx))
		if err != nil {
			if domainUnavailable(err) {
				return nil, provider.ErrDomainUnavailable
			}
			return nil, fmt.Errorf("gitlab: groups: %w", err)
		}
		for _, g := range groups {
			out = append(out, provider.Group{
				ID:       strconv.FormatInt(g.ID, 10),
				FullPath: g.FullPath,
				Path:     g.Path,
				Name:     g.Name,
				ParentID: idString(g.ParentID),
			})
		}
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return out, nil
}

// ListGroupProjects returns the projects of one group, subgroups included (all
// pages). A 403/404 is reported as provider.ErrDomainUnavailable.
func (c *Client) ListGroupProjects(ctx context.Context, groupPath string) ([]provider.Project, error) {
	opt := &gitlab.ListGroupProjectsOptions{
		ListOptions:      gitlab.ListOptions{PerPage: 100},
		IncludeSubGroups: gitlab.Ptr(true),
	}
	var out []provider.Project
	for {
		if err := waitRate(ctx, c.projectRate); err != nil {
			return nil, err
		}
		projects, resp, err := c.api.Groups.ListGroupProjects(groupPath, opt, gitlab.WithContext(ctx))
		if err != nil {
			if domainUnavailable(err) {
				return nil, provider.ErrDomainUnavailable
			}
			return nil, fmt.Errorf("gitlab: group projects %s: %w", groupPath, err)
		}
		for _, p := range projects {
			out = append(out, provider.Project{
				ID:            strconv.FormatInt(p.ID, 10),
				FullPath:      p.PathWithNamespace,
				Path:          p.Path,
				Name:          p.Name,
				NamespaceKind: namespaceKind(p),
				CreatorID:     idString(p.CreatorID),
			})
		}
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return out, nil
}

// idString renders a GitLab numeric id as the normalized string form; 0 means
// the field is absent (no parent/creator) and maps to "".
func idString(id int64) string {
	if id == 0 {
		return ""
	}
	return strconv.FormatInt(id, 10)
}

// namespaceKind reads the project namespace kind ("user" | "group"); an absent
// namespace (older instances/simple payloads) yields "".
func namespaceKind(p *gitlab.Project) string {
	if p.Namespace == nil {
		return ""
	}
	return p.Namespace.Kind
}
