// GitLab merge-request nodes (T024/US2): the MR list carrying the merge
// metadata and the diff versions of one MR — the merge_request /
// merge_request_diff graph nodes.
package gitlab

import (
	"context"
	"fmt"
	"strconv"

	gitlab "gitlab.com/gitlab-org/api/client-go/v3"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// ListMergeRequestNodes returns the project MRs with their merge metadata (all
// pages, all states): a merge_request node must know whether and by which
// commit the MR merged, not only the pre-merge fields. A 403/404 is reported
// as provider.ErrDomainUnavailable.
func (c *Client) ListMergeRequestNodes(ctx context.Context, repo provider.Repo) ([]provider.ChangeRequest, error) {
	pid := repoFullPath(repo)
	opt := &gitlab.ListProjectMergeRequestsOptions{
		ListOptions: gitlab.ListOptions{PerPage: 100},
		State:       gitlab.Ptr("all"),
	}
	var out []provider.ChangeRequest
	for {
		mrs, resp, err := c.api.MergeRequests.ListProjectMergeRequests(pid, opt, gitlab.WithContext(ctx))
		if err != nil {
			if domainUnavailable(err) {
				return nil, provider.ErrDomainUnavailable
			}
			return nil, fmt.Errorf("gitlab: merge request nodes %s: %w", pid, err)
		}
		for _, mr := range mrs {
			var author string
			if mr.Author != nil {
				author = mr.Author.Username
			}
			out = append(out, provider.ChangeRequest{
				ID:             mr.IID,
				Title:          mr.Title,
				Body:           mr.Description,
				State:          provider.NormalizeState(mr.State),
				SourceBranch:   mr.SourceBranch,
				TargetBranch:   mr.TargetBranch,
				URL:            mr.WebURL,
				UpdatedAt:      timeOrZero(mr.UpdatedAt),
				Author:         author,
				Assignees:      usernames(mr.Assignees),
				Reviewers:      usernames(mr.Reviewers),
				Labels:         []string(mr.Labels),
				Milestone:      milestoneTitle(mr.Milestone),
				Merged:         mr.State == "merged",
				MergeCommitSHA: mr.MergeCommitSHA,
			})
		}
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return out, nil
}

// ListMergeRequestDiffRefs returns the diff versions of one MR (all pages):
// each version's base/start/head SHAs and state, the merge_request_diff nodes.
// Versions are fetched per request on demand; a 403/404 is reported as
// provider.ErrDomainUnavailable.
func (c *Client) ListMergeRequestDiffRefs(ctx context.Context, repo provider.Repo, mrID int) ([]provider.MergeRequestDiffRef, error) {
	pid := repoFullPath(repo)
	opt := &gitlab.GetMergeRequestDiffVersionsOptions{ListOptions: gitlab.ListOptions{PerPage: 100}}
	var out []provider.MergeRequestDiffRef
	for {
		vs, resp, err := c.api.MergeRequests.GetMergeRequestDiffVersions(pid, int64(mrID), opt, gitlab.WithContext(ctx))
		if err != nil {
			if domainUnavailable(err) {
				return nil, provider.ErrDomainUnavailable
			}
			return nil, fmt.Errorf("gitlab: MR diff refs %s!%d: %w", pid, mrID, err)
		}
		for _, v := range vs {
			out = append(out, provider.MergeRequestDiffRef{
				Version:  strconv.FormatInt(v.ID, 10),
				BaseSHA:  v.BaseCommitSHA,
				StartSHA: v.StartCommitSHA,
				HeadSHA:  v.HeadCommitSHA,
				State:    v.State,
			})
		}
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return out, nil
}
