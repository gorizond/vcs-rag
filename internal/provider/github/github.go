// Package github implements provider.VCSProvider for GitHub (FR-003).
package github

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v92/github"

	"github.com/gorizond/vcs-rag/internal/config"
	"github.com/gorizond/vcs-rag/internal/provider"
)

// Client is the GitHub provider: lists organization repositories unioned with
// explicitly configured repositories.
type Client struct {
	api      *gh.Client
	orgs     []string
	repos    []string
	ciWindow int
	logsHTTP *http.Client
	upstream *upstreamTransport
}

// New creates the GitHub client; it makes no network calls.
// A custom GITHUB_BASE_URL (GHES) is wired via WithEnterpriseURLs.
// T143: the token is optional (public organizations) — WithAuthToken("")
// is an error in go-github, and an anonymous client is exactly the tokenless
// one; the option is added only for a non-empty token.
func New(cfg *config.Config) (*Client, error) {
	tr := &upstreamTransport{base: http.DefaultTransport}
	tr.gate.Store(provider.NewUpstreamGate(cfg.Worker.UpstreamConcurrency))
	opts := []gh.ClientOptionsFunc{gh.WithHTTPClient(&http.Client{Transport: tr})}
	if cfg.Provider.GitHubToken != "" {
		opts = append(opts, gh.WithAuthToken(cfg.Provider.GitHubToken))
	}
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
		repos:    cfg.Provider.GitHubRepos,
		ciWindow: ciWindow,
		// Actions log download goes via the signed redirect URL, without Authorization.
		logsHTTP: &http.Client{Timeout: 60 * time.Second},
		upstream: tr,
	}, nil
}

// SetUpstreamGate shares the instance-wide upstream semaphore with the worker
// (FR-002): git clone/fetch and API traffic draw from one gate.
func (c *Client) SetUpstreamGate(g *provider.UpstreamGate) {
	if g != nil {
		c.upstream.gate.Store(g)
	}
}

// SetUpstreamQueue shares the bounded FR-011 queue with the worker: an API
// request over capacity fails with provider.ErrTooManyTasks instead of waiting
// indefinitely (T063).
func (c *Client) SetUpstreamQueue(q *provider.UpstreamQueue) {
	if q != nil {
		c.upstream.queue.Store(q)
	}
}

// SetPageCharger installs the per-page budget hook (FR-003); nil removes it.
func (c *Client) SetPageCharger(fn func()) { c.upstream.setPageCharger(fn) }

// RateUsage exposes the authenticated budget consumption from the last
// X-RateLimit-* headers (FR-015); ok=false — headers were absent.
func (c *Client) RateUsage() (float64, bool) { return c.upstream.usage() }

// Pages returns the API page responses served since start (FR-003).
func (c *Client) Pages() int64 { return c.upstream.pages.Load() }

// RateLimitEvents returns the rate-limit responses observed since start
// (primary 403 + secondary 429; FR-008).
func (c *Client) RateLimitEvents() int64 { return c.upstream.rateEvents.Load() }

// LastRetryAfter returns the last Retry-After carried by a 429 (FR-008).
func (c *Client) LastRetryAfter() time.Duration {
	return time.Duration(c.upstream.retry.Load())
}

// Type returns "github".
func (c *Client) Type() string { return "github" }

// ListRepositories returns repositories of all configured organizations
// (full pagination) unioned with the explicit GITHUB_REPO entries,
// deduplicated by ID.
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
	// The allowlist: explicit owner/repo references are fetched one by one.
	// A repository that cannot be fetched (404/403) is logged and skipped —
	// never fatal.
	for _, ref := range c.repos {
		owner, name, ok := strings.Cut(ref, "/")
		if !ok || owner == "" || name == "" {
			slog.Warn("github: skipping invalid repo reference", "repo", ref)
			continue
		}
		r, _, err := c.api.Repositories.Get(ctx, owner, name)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			slog.Warn("github: skipping repo", "repo", ref, "err", err)
			continue
		}
		ns := owner
		if login := r.GetOwner().GetLogin(); login != "" {
			ns = login
		}
		all = append(all, provider.Repo{
			ID:            strconv.FormatInt(r.GetID(), 10),
			Namespace:     ns,
			Path:          r.GetName(),
			CloneURL:      r.GetCloneURL(),
			DefaultBranch: r.GetDefaultBranch(),
		})
	}
	return provider.MergeRepos(all), nil
}
