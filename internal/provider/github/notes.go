// GitHub issue/PR comments (T021/US2): the comment list of one issue or pull
// request, mapped onto the normalized provider.Note shape.
package github

import (
	"context"
	"fmt"
	"strconv"

	gh "github.com/google/go-github/v92/github"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// notesPerPage is the page size of the comment endpoints (GitHub caps per_page
// at 100).
const notesPerPage = 100

// ListNotes returns all comments of one issue or change request, all pages.
// GitHub splits change-request comments across two endpoints — the issue
// timeline (/issues/{n}/comments) and the review comments
// (/pulls/{n}/comments) — both are returned. GitHub surfaces no system notes:
// System is always false. A 403/404 is the domain being off (private data,
// missing scope) — reported as provider.ErrDomainUnavailable.
func (c *Client) ListNotes(ctx context.Context, repo provider.Repo, parentKind string, parentID int) ([]provider.Note, error) {
	switch parentKind {
	case "issue":
		return c.issueComments(ctx, repo, parentKind, parentID)
	case "change_request":
		notes, err := c.issueComments(ctx, repo, parentKind, parentID)
		if err != nil {
			return nil, err
		}
		review, err := c.reviewComments(ctx, repo, parentKind, parentID)
		if err != nil {
			return nil, err
		}
		return append(notes, review...), nil
	default:
		return nil, fmt.Errorf("github: notes %s#%d: unsupported parent kind %q", repo.Path, parentID, parentKind)
	}
}

// issueComments pages through the issue/PR timeline comments.
func (c *Client) issueComments(ctx context.Context, repo provider.Repo, parentKind string, parentID int) ([]provider.Note, error) {
	opt := &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: notesPerPage}}
	var out []provider.Note
	for {
		comments, resp, err := c.api.Issues.ListComments(ctx, repo.Namespace, repo.Path, parentID, opt)
		if err != nil {
			return nil, commentErr(repo, parentID, err)
		}
		for _, cm := range comments {
			out = append(out, provider.Note{
				ID:          cm.GetID(),
				ParentKind:  parentKind,
				ParentID:    parentID,
				AuthorID:    strconv.FormatInt(cm.GetUser().GetID(), 10),
				AuthorLogin: cm.GetUser().GetLogin(),
				Body:        cm.GetBody(),
				CreatedAt:   cm.GetCreatedAt().Time,
				UpdatedAt:   cm.GetUpdatedAt().Time,
			})
		}
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return out, nil
}

// reviewComments pages through the PR review comments (/pulls/{n}/comments).
func (c *Client) reviewComments(ctx context.Context, repo provider.Repo, parentKind string, parentID int) ([]provider.Note, error) {
	opt := &gh.PullRequestListCommentsOptions{ListOptions: gh.ListOptions{PerPage: notesPerPage}}
	var out []provider.Note
	for {
		comments, resp, err := c.api.PullRequests.ListComments(ctx, repo.Namespace, repo.Path, parentID, opt)
		if err != nil {
			return nil, commentErr(repo, parentID, err)
		}
		for _, cm := range comments {
			out = append(out, provider.Note{
				ID:          cm.GetID(),
				ParentKind:  parentKind,
				ParentID:    parentID,
				AuthorID:    strconv.FormatInt(cm.GetUser().GetID(), 10),
				AuthorLogin: cm.GetUser().GetLogin(),
				Body:        cm.GetBody(),
				CreatedAt:   cm.GetCreatedAt().Time,
				UpdatedAt:   cm.GetUpdatedAt().Time,
			})
		}
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return out, nil
}

// commentErr maps a 403/404 onto the domain-unavailable sentinel (the same
// capability gate as the security endpoints) and wraps anything else.
func commentErr(repo provider.Repo, parentID int, err error) error {
	if isSecurityUnavailable(err) {
		return provider.ErrDomainUnavailable
	}
	return fmt.Errorf("github: comments %s#%d: %w", repo.Path, parentID, err)
}
