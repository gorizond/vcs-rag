// Package github implements provider.VCSProvider for GitHub (FR-003).
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

// Client is the GitHub provider: lists organization repositories.
type Client struct {
	api      *gh.Client
	orgs     []string
	ciWindow int
	logsHTTP *http.Client
}

// New creates the GitHub client; it makes no network calls.
// A custom GITHUB_BASE_URL (GHES) is wired via WithEnterpriseURLs.
func New(cfg *config.Config) (*Client, error) {
	opts := []gh.ClientOptionsFunc{gh.WithAuthToken(cfg.Provider.GitHubToken)}
	if base := cfg.Provider.GitHubBaseURL; base != "" && base != "https://api.github.com" {
		opts = append(opts, gh.WithEnterpriseURLs(base, base))
	}
	api, err := gh.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("github client: %w", err)
	}
	ciWindow := cfg.Worker.CIWindow
	if ciWindow < 1 {
		ciWindow = 50 // ponytail: fallback in case the config is built bypassing Load
	}
	return &Client{
		api:      api,
		orgs:     cfg.Provider.GitHubOrgs,
		ciWindow: ciWindow,
		// Actions log download goes via the signed redirect URL, without Authorization.
		logsHTTP: &http.Client{Timeout: 60 * time.Second},
	}, nil
}

// Type returns "github".
func (c *Client) Type() string { return "github" }

// ListRepositories returns repositories of all configured organizations
// (full pagination), deduplicated by ID.
func (c *Client) ListRepositories(ctx context.Context) ([]provider.Repo, error) {
	var all []provider.Repo
	for _, org := range c.orgs {
		opt := &gh.RepositoryListByOrgOptions{ListOptions: gh.ListOptions{PerPage: 100}}
		for {
			repos, resp, err := c.api.Repositories.ListByOrg(ctx, org, opt)
			if err != nil {
				return nil, fmt.Errorf("organization %s: %w", org, err)
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
