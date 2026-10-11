// Package gitlab implements provider.VCSProvider for GitLab (FR-003).
package gitlab

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	gitlab "gitlab.com/gitlab-org/api/client-go/v3"

	"github.com/gorizond/vcs-rag/internal/config"
	"github.com/gorizond/vcs-rag/internal/provider"
)

// Client is the GitLab provider: lists group projects (including subgroups)
// unioned with explicitly configured projects.
type Client struct {
	api      *gitlab.Client
	groups   []string
	projects []string
	ciWindow int
}

// New creates the GitLab client; it makes no network calls.
func New(cfg *config.Config) (*Client, error) {
	api, err := gitlab.NewClient(cfg.Provider.GitLabToken, gitlab.WithBaseURL(cfg.Provider.GitLabBaseURL))
	if err != nil {
		return nil, fmt.Errorf("gitlab client: %w", err)
	}
	ciWindow := cfg.Worker.CIWindow
	if ciWindow < 1 {
		ciWindow = 50 // ponytail: fallback in case the config is built bypassing Load
	}
	return &Client{
		api:      api,
		groups:   cfg.Provider.GitLabGroups,
		projects: cfg.Provider.GitLabProjects,
		ciWindow: ciWindow,
	}, nil
}

// Type returns "gitlab".
func (c *Client) Type() string { return "gitlab" }

// ListRepositories returns projects of all configured groups (IncludeSubGroups,
// full pagination) unioned with the explicit GITLAB_PROJECT_ID entries,
// deduplicated by ID.
func (c *Client) ListRepositories(ctx context.Context) ([]provider.Repo, error) {
	var all []provider.Repo
	for _, gid := range c.groups {
		opt := &gitlab.ListGroupProjectsOptions{
			ListOptions:      gitlab.ListOptions{PerPage: 100},
			IncludeSubGroups: gitlab.Ptr(true),
		}
		for {
			projects, resp, err := c.api.Groups.ListGroupProjects(gid, opt, gitlab.WithContext(ctx))
			if err != nil {
				return nil, fmt.Errorf("group %s: %w", gid, err)
			}
			for _, p := range projects {
				all = append(all, repoFromProject(p))
			}
			if resp.NextPage == 0 {
				break
			}
			opt.Page = resp.NextPage
		}
	}
	// The allowlist: a numeric ID or a full path (group/sub/project) — the
	// client URL-escapes the string and GitLab resolves both forms on the same
	// endpoint. A project that cannot be fetched (404/403) is logged and
	// skipped — never fatal.
	for _, ref := range c.projects {
		p, _, err := c.api.Projects.GetProject(ref, nil, gitlab.WithContext(ctx))
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			slog.Warn("gitlab: skipping project", "project", ref, "err", err)
			continue
		}
		all = append(all, repoFromProject(p))
	}
	return provider.MergeRepos(all), nil
}

// repoFromProject normalizes a GitLab project.
func repoFromProject(p *gitlab.Project) provider.Repo {
	ns := ""
	if p.Namespace != nil {
		ns = p.Namespace.FullPath
	}
	return provider.Repo{
		ID:            strconv.FormatInt(p.ID, 10),
		Namespace:     ns,
		Path:          p.Path,
		CloneURL:      p.HTTPURLToRepo,
		DefaultBranch: p.DefaultBranch,
	}
}
