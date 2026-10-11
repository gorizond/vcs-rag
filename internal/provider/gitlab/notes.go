// GitLab notes/comments (T020/US2): the note list of one issue or merge
// request, mapped onto the normalized provider.Note shape.
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

// notesPerPage is the page size of the notes endpoints (GitLab caps per_page
// at 100).
const notesPerPage = 100

// ListNotes returns all notes (comments) of one issue or change request, all
// pages; system notes are surfaced too (Note.System). A 403/404 is the domain
// being off (token without read_api, no project access) — reported as
// provider.ErrDomainUnavailable, not as a pass failure.
func (c *Client) ListNotes(ctx context.Context, repo provider.Repo, parentKind string, parentID int) ([]provider.Note, error) {
	pid := repoFullPath(repo)
	fetch := func(page int64) ([]*gitlab.Note, *gitlab.Response, error) {
		switch parentKind {
		case "issue":
			return c.api.Notes.ListIssueNotes(pid, int64(parentID),
				&gitlab.ListIssueNotesOptions{ListOptions: gitlab.ListOptions{PerPage: notesPerPage, Page: page}},
				gitlab.WithContext(ctx))
		case "change_request":
			return c.api.Notes.ListMergeRequestNotes(pid, int64(parentID),
				&gitlab.ListMergeRequestNotesOptions{ListOptions: gitlab.ListOptions{PerPage: notesPerPage, Page: page}},
				gitlab.WithContext(ctx))
		default:
			return nil, nil, fmt.Errorf("unsupported parent kind %q", parentKind)
		}
	}
	var out []provider.Note
	for page := int64(1); ; {
		notes, resp, err := fetch(page)
		if err != nil {
			if isDomainUnavailable(err) {
				return nil, provider.ErrDomainUnavailable
			}
			return nil, fmt.Errorf("gitlab: notes %s#%d: %w", pid, parentID, err)
		}
		for _, n := range notes {
			out = append(out, noteFromGitLab(n, parentKind, parentID))
		}
		if resp.NextPage == 0 {
			break
		}
		page = resp.NextPage
	}
	return out, nil
}

// noteFromGitLab maps one GitLab note; AuthorID is the numeric user id — the
// stable identity the user node keys on (logins rename).
func noteFromGitLab(n *gitlab.Note, parentKind string, parentID int) provider.Note {
	return provider.Note{
		ID:          n.ID,
		ParentKind:  parentKind,
		ParentID:    parentID,
		AuthorID:    strconv.FormatInt(n.Author.ID, 10),
		AuthorLogin: n.Author.Username,
		Body:        n.Body,
		System:      n.System,
		CreatedAt:   timeOrZero(n.CreatedAt),
		UpdatedAt:   timeOrZero(n.UpdatedAt),
	}
}

// isDomainUnavailable reports whether err is a provider 403/404: the notes/users
// domain is off (missing scope or no project access) — a capability signal, not
// a failure (provider.ErrDomainUnavailable).
func isDomainUnavailable(err error) bool {
	var er *gitlab.ErrorResponse
	return errors.As(err, &er) &&
		(er.HasStatusCode(http.StatusForbidden) || er.HasStatusCode(http.StatusNotFound))
}
