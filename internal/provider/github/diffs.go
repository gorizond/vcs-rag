// GitHub PR file diffs (T023/US2): the per-file diff list of one pull request.
package github

import (
	"context"
	"fmt"

	gh "github.com/google/go-github/v92/github"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// prFilesMax is GitHub's hard cap of files returned for one pull request; the
// endpoint returns only the first 3000 files of a larger PR.
const prFilesMax = 3000

// ListChangeRequestDiffs returns the diffs of the files changed by one PR
// (paginated, per_page ≤ 100; the scan stops at prFilesMax). GitHub omits
// `patch` for files whose diff is too large or binary — such a file is marked
// truncated: the stored diff is incomplete, whatever the reason.
func (c *Client) ListChangeRequestDiffs(ctx context.Context, repo provider.Repo, mrID int) ([]provider.ChangeRequestFile, error) {
	opt := &gh.ListOptions{PerPage: 100}
	var out []provider.ChangeRequestFile
	for {
		files, resp, err := c.api.PullRequests.ListFiles(ctx, repo.Namespace, repo.Path, mrID, opt)
		if err != nil {
			return nil, fmt.Errorf("github: PR files %s#%d: %w", repo.Path, mrID, err)
		}
		for _, f := range files {
			out = append(out, provider.ChangeRequestFile{
				OldPath:   previousPath(f),
				NewPath:   f.GetFilename(),
				Status:    ghFileStatus(f.GetStatus()),
				Patch:     f.GetPatch(),
				Truncated: f.Patch == nil,
				MRID:      mrID,
			})
			if len(out) >= prFilesMax {
				return out, nil
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return out, nil
}

// ghFileStatus maps the GitHub file status (added | removed | modified |
// renamed | copied | changed | unchanged) to the normalized vocabulary.
func ghFileStatus(status string) string {
	switch status {
	case "added":
		return "added"
	case "removed":
		return "deleted"
	case "renamed":
		return "renamed"
	default:
		return "modified"
	}
}

// previousPath is the pre-rename path; without a rename it is the path itself
// (both providers fill OldPath, GitLab-style).
func previousPath(f *gh.CommitFile) string {
	if p := f.GetPreviousFilename(); p != "" {
		return p
	}
	return f.GetFilename()
}
