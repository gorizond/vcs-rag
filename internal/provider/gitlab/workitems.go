// GitLab work items (T028/US3): GitLab has NO REST work_items endpoint — the
// Issues API is the work-item source, and its issue_type column distinguishes
// issues from tasks (incidents/test cases pass through as they come).
package gitlab

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	gitlab "gitlab.com/gitlab-org/api/client-go/v3"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// domainUnavailable reports a GitLab 403/404: the domain (work items,
// milestones, labels) is disabled or the token lacks the scope — a capability
// signal (provider.ErrDomainUnavailable), not a failure. GitLab rate limits
// answer 429, so a 403 is never a transient throttle here.
func domainUnavailable(err error) bool {
	var er *gitlab.ErrorResponse
	return errors.As(err, &er) &&
		(er.HasStatusCode(http.StatusForbidden) || er.HasStatusCode(http.StatusNotFound))
}

// workItemType maps the GitLab issue_type onto the normalized item type; an
// absent type (older instances) is an issue.
func workItemType(t *string) string {
	if t == nil || *t == "" {
		return "issue"
	}
	return *t
}

// issueAuthorID — the numeric id of an issue author ("" — unknown) as a
// string: the graph user key is id-based, not login-based.
func issueAuthorID(a *gitlab.IssueAuthor) string {
	if a == nil {
		return ""
	}
	return strconv.FormatInt(a.ID, 10)
}

// issueAssigneeIDs — the numeric ids of issue assignees (nil-safe).
func issueAssigneeIDs(users []*gitlab.IssueAssignee) []string {
	out := make([]string, 0, len(users))
	for _, u := range users {
		if u != nil {
			out = append(out, strconv.FormatInt(u.ID, 10))
		}
	}
	return out
}

// issueMilestoneID — the milestone id of an issue (0 — none).
func issueMilestoneID(m *gitlab.Milestone) int {
	if m == nil {
		return 0
	}
	return int(m.ID)
}

// ListWorkItems returns the project work items (all pages) via the Issues API;
// a 403/404 is reported as provider.ErrDomainUnavailable.
func (c *Client) ListWorkItems(ctx context.Context, repo provider.Repo) ([]provider.WorkItem, error) {
	pid := repoFullPath(repo)
	opt := &gitlab.ListProjectIssuesOptions{ListOptions: gitlab.ListOptions{PerPage: 100}}
	var out []provider.WorkItem
	for {
		issues, resp, err := c.api.Issues.ListProjectIssues(pid, opt, gitlab.WithContext(ctx))
		if err != nil {
			if domainUnavailable(err) {
				return nil, provider.ErrDomainUnavailable
			}
			return nil, fmt.Errorf("gitlab: work items %s: %w", pid, err)
		}
		for _, iss := range issues {
			out = append(out, provider.WorkItem{
				ID:          int(iss.IID),
				ItemType:    workItemType(iss.IssueType),
				State:       provider.NormalizeState(iss.State),
				Title:       iss.Title,
				UpdatedAt:   timeOrZero(iss.UpdatedAt),
				AuthorID:    issueAuthorID(iss.Author),
				AssigneeIDs: issueAssigneeIDs(iss.Assignees),
				LabelNames:  []string(iss.Labels),
				MilestoneID: issueMilestoneID(iss.Milestone),
			})
		}
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return out, nil
}
