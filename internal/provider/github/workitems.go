// GitHub work items (T029/US3): the repository issues behind the work_item
// nodes; GitHub has no separate work-item type, every issue is item_type
// "issue".
package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	gh "github.com/google/go-github/v92/github"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// domainUnavailable reports a GitHub 403/404 on a repository-scoped domain
// (issues, milestones, labels): the feature is off or the token lacks the
// scope — a capability signal (provider.ErrDomainUnavailable). A rate-limit
// 403 is transient, not a capability signal, and stays an error.
func domainUnavailable(err error) bool {
	var rl *gh.RateLimitError
	if errors.As(err, &rl) {
		return false
	}
	var er *gh.ErrorResponse
	return errors.As(err, &er) && er.Response != nil &&
		(er.Response.StatusCode == http.StatusForbidden || er.Response.StatusCode == http.StatusNotFound)
}

// userID — the numeric id of a user ("" — unknown) as a string: the graph user
// key is id-based, not login-based.
func userID(u *gh.User) string {
	if u == nil {
		return ""
	}
	return strconv.FormatInt(u.GetID(), 10)
}

// userIDs — the numeric ids of users (nil-safe).
func userIDs(users []*gh.User) []string {
	out := make([]string, 0, len(users))
	for _, u := range users {
		if u != nil {
			out = append(out, strconv.FormatInt(u.GetID(), 10))
		}
	}
	return out
}

// ListWorkItems returns the repository issues (all pages, all states); a
// 403/404 is reported as provider.ErrDomainUnavailable. The milestone number
// is the repo-local id MilestonePoint keys the milestone node on.
func (c *Client) ListWorkItems(ctx context.Context, repo provider.Repo) ([]provider.WorkItem, error) {
	opt := &gh.IssueListByRepoOptions{State: "all", ListOptions: gh.ListOptions{PerPage: 100}}
	var out []provider.WorkItem
	for {
		issues, resp, err := c.api.Issues.ListByRepo(ctx, repo.Namespace, repo.Path, opt)
		if err != nil {
			if domainUnavailable(err) {
				return nil, provider.ErrDomainUnavailable
			}
			return nil, fmt.Errorf("github: work items %s: %w", repo.Path, err)
		}
		for _, iss := range issues {
			out = append(out, provider.WorkItem{
				ID:          iss.GetNumber(),
				ItemType:    "issue",
				State:       provider.NormalizeState(iss.GetState()),
				Title:       iss.GetTitle(),
				UpdatedAt:   iss.GetUpdatedAt().Time,
				AuthorID:    userID(iss.GetUser()),
				AssigneeIDs: userIDs(iss.Assignees),
				LabelNames:  labelNames(iss.Labels),
				MilestoneID: iss.GetMilestone().GetNumber(),
			})
		}
		if resp.NextPage == 0 {
			break
		}
		opt.ListOptions.Page = resp.NextPage
	}
	return out, nil
}
