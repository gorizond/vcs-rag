// Package gitlab — реализация provider.VCSProvider для GitLab (FR-003).
package gitlab

import (
	"context"
	"fmt"
	"strconv"

	gitlab "gitlab.com/gitlab-org/api/client-go/v3"

	"github.com/gorizond/vcs-rag/internal/config"
	"github.com/gorizond/vcs-rag/internal/provider"
)

// Client — GitLab-провайдер: листинг проектов групп (включая подгруппы).
type Client struct {
	api      *gitlab.Client
	groups   []string
	ciWindow int
}

// New создаёт клиент GitLab; сетевых вызовов не выполняет.
func New(cfg *config.Config) (*Client, error) {
	api, err := gitlab.NewClient(cfg.Provider.GitLabToken, gitlab.WithBaseURL(cfg.Provider.GitLabBaseURL))
	if err != nil {
		return nil, fmt.Errorf("gitlab-клиент: %w", err)
	}
	ciWindow := cfg.Worker.CIWindow
	if ciWindow < 1 {
		ciWindow = 50 // ponytail: фолбэк, если конфиг собран в обход Load
	}
	return &Client{api: api, groups: cfg.Provider.GitLabGroups, ciWindow: ciWindow}, nil
}

// Type возвращает "gitlab".
func (c *Client) Type() string { return "gitlab" }

// ListRepositories возвращает проекты всех групп конфига (IncludeSubGroups,
// пагинация до конца), дедуплицированные по ID.
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
				return nil, fmt.Errorf("группа %s: %w", gid, err)
			}
			for _, p := range projects {
				ns := ""
				if p.Namespace != nil {
					ns = p.Namespace.FullPath
				}
				all = append(all, provider.Repo{
					ID:            strconv.FormatInt(p.ID, 10),
					Namespace:     ns,
					Path:          p.Path,
					CloneURL:      p.HTTPURLToRepo,
					DefaultBranch: p.DefaultBranch,
				})
			}
			if resp.NextPage == 0 {
				break
			}
			opt.Page = resp.NextPage
		}
	}
	return provider.MergeRepos(all), nil
}
