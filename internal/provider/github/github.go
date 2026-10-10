// Package github — реализация provider.VCSProvider для GitHub (FR-003).
package github

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	gh "github.com/google/go-github/v92/github"

	"github.com/gorizond/vcs-rag/internal/config"
	"github.com/gorizond/vcs-rag/internal/provider"
)

// Client — GitHub-провайдер: листинг репозиториев организаций.
type Client struct {
	api      *gh.Client
	orgs     []string
	ciWindow int
	logsHTTP *http.Client
}

// New создаёт клиент GitHub; сетевых вызовов не выполняет.
// Кастомный GITHUB_BASE_URL (GHES) подключается через WithEnterpriseURLs.
func New(cfg *config.Config) (*Client, error) {
	opts := []gh.ClientOptionsFunc{gh.WithAuthToken(cfg.Provider.GitHubToken)}
	if base := cfg.Provider.GitHubBaseURL; base != "" && base != "https://api.github.com" {
		opts = append(opts, gh.WithEnterpriseURLs(base, base))
	}
	api, err := gh.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("github-клиент: %w", err)
	}
	ciWindow := cfg.Worker.CIWindow
	if ciWindow < 1 {
		ciWindow = 50 // ponytail: фолбэк, если конфиг собран в обход Load
	}
	return &Client{
		api:      api,
		orgs:     cfg.Provider.GitHubOrgs,
		ciWindow: ciWindow,
		// Загрузка логов Actions — по подписанному redirect URL, без Authorization.
		logsHTTP: &http.Client{Timeout: 60 * time.Second},
	}, nil
}

// Type возвращает "github".
func (c *Client) Type() string { return "github" }

// ListRepositories возвращает репозитории всех организаций конфига
// (пагинация до конца), дедуплицированные по ID.
func (c *Client) ListRepositories(ctx context.Context) ([]provider.Repo, error) {
	var all []provider.Repo
	for _, org := range c.orgs {
		opt := &gh.RepositoryListByOrgOptions{ListOptions: gh.ListOptions{PerPage: 100}}
		for {
			repos, resp, err := c.api.Repositories.ListByOrg(ctx, org, opt)
			if err != nil {
				return nil, fmt.Errorf("организация %s: %w", org, err)
			}
			for _, r := range repos {
				all = append(all, provider.Repo{
					ID:            strconv.FormatInt(r.GetID(), 10),
					Namespace:     org,
					Path:          r.GetName(),
					CloneURL:      r.GetCloneURL(),
					DefaultBranch: r.GetDefaultBranch(),
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
