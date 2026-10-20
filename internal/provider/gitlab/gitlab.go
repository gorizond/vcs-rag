// Package gitlab implements provider.VCSProvider for GitLab (FR-003).
package gitlab

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	gitlab "gitlab.com/gitlab-org/api/client-go/v3"

	"github.com/gorizond/vcs-rag/internal/config"
	"github.com/gorizond/vcs-rag/internal/provider"
)

// Client is the GitLab provider: lists group projects (including subgroups)
// unioned with explicitly configured projects.
type Client struct {
	api         *gitlab.Client
	groups      []string
	projects    []string
	ciWindow    int
	groupRate   *tokenBucket // GET /groups: 50/min (R2)
	projectRate *tokenBucket // project listing: 600/min (R2)
	upstream    *upstreamTransport
}

// New creates the GitLab client; it makes no network calls.
func New(cfg *config.Config) (*Client, error) {
	tr := &upstreamTransport{base: http.DefaultTransport}
	tr.gate.Store(provider.NewUpstreamGate(cfg.Worker.UpstreamConcurrency))
	api, err := gitlab.NewClient(cfg.Provider.GitLabToken,
		gitlab.WithBaseURL(cfg.Provider.GitLabBaseURL),
		gitlab.WithHTTPClient(&http.Client{Transport: tr}))
	if err != nil {
		return nil, fmt.Errorf("gitlab client: %w", err)
	}
	ciWindow := cfg.Worker.CIWindow
	if ciWindow < 1 {
		ciWindow = 50 // ponytail: fallback in case the config is built bypassing Load
	}
	return &Client{
		api:         api,
		groups:      cfg.Provider.GitLabGroups,
		projects:    cfg.Provider.GitLabProjects,
		ciWindow:    ciWindow,
		groupRate:   newTokenBucket(groupsPerMinute),
		projectRate: newTokenBucket(projectsPerMinute),
		upstream:    tr,
	}, nil
}

// SetUpstreamGate shares the instance-wide upstream semaphore with the worker
// (FR-002): git clone/fetch and API traffic then draw from one gate.
func (c *Client) SetUpstreamGate(g *provider.UpstreamGate) {
	if g != nil {
		c.upstream.gate.Store(g)
	}
}

// RateUsage exposes the authenticated budget consumption from the last
// RateLimit-* headers (FR-015); ok=false — headers were absent.
func (c *Client) RateUsage() (float64, bool) { return c.upstream.usage() }

// Pages returns the API page responses served since start (FR-003).
func (c *Client) Pages() int64 { return c.upstream.pages.Load() }

// RateLimitEvents returns the 429 responses observed since start (FR-008).
func (c *Client) RateLimitEvents() int64 { return c.upstream.four29s.Load() }

// LastRetryAfter returns the last Retry-After carried by a 429 (FR-008).
func (c *Client) LastRetryAfter() time.Duration {
	return time.Duration(c.upstream.retry.Load())
}

// SetPageCharger installs the per-page budget hook (FR-003); nil removes it.
func (c *Client) SetPageCharger(fn func()) { c.upstream.setPageCharger(fn) }

// waitRate applies the narrow-endpoint cap before a request (FR-002).
func waitRate(ctx context.Context, bucket *tokenBucket) error {
	if bucket == nil {
		return nil
	}
	return bucket.wait(ctx)
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
			if err := waitRate(ctx, c.groupRate); err != nil {
				return nil, err
			}
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
		if err := waitRate(ctx, c.projectRate); err != nil {
			return nil, err
		}
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
