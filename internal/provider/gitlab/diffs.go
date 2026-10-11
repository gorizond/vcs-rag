// GitLab MR file diffs (T022/US2): the per-file diff list of one merge request.
package gitlab

import (
	"context"
	"fmt"

	gitlab "gitlab.com/gitlab-org/api/client-go/v3"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// ListChangeRequestDiffs returns the diffs of the files changed by one MR
// (all pages). A file GitLab could not render (too large or collapsed) carries
// no full diff — the provider flags it and it becomes patch_truncated.
func (c *Client) ListChangeRequestDiffs(ctx context.Context, repo provider.Repo, mrID int) ([]provider.ChangeRequestFile, error) {
	pid := repoFullPath(repo)
	opt := &gitlab.ListMergeRequestDiffsOptions{ListOptions: gitlab.ListOptions{PerPage: 100}}
	var out []provider.ChangeRequestFile
	for {
		diffs, resp, err := c.api.MergeRequests.ListMergeRequestDiffs(pid, int64(mrID), opt, gitlab.WithContext(ctx))
		if err != nil {
			return nil, fmt.Errorf("gitlab: MR diffs %s!%d: %w", pid, mrID, err)
		}
		for _, d := range diffs {
			out = append(out, provider.ChangeRequestFile{
				OldPath:   d.OldPath,
				NewPath:   d.NewPath,
				Status:    diffStatus(d.NewFile, d.DeletedFile, d.RenamedFile),
				Patch:     d.Diff,
				Truncated: d.TooLarge || d.Collapsed,
				MRID:      mrID,
			})
		}
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return out, nil
}

// diffStatus maps the GitLab file flags to the normalized vocabulary.
func diffStatus(newFile, deletedFile, renamedFile bool) string {
	switch {
	case newFile:
		return "added"
	case deletedFile:
		return "deleted"
	case renamedFile:
		return "renamed"
	default:
		return "modified"
	}
}
