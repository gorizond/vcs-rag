package store

import (
	"context"
	"fmt"

	"github.com/qdrant/go-client/qdrant"
)

// DeleteByFilter deletes points by filter (Wait=true).
func (s *Store) DeleteByFilter(ctx context.Context, f *qdrant.Filter) error {
	if _, err := s.client.Delete(ctx, &qdrant.DeletePoints{
		CollectionName: s.collection,
		Points:         qdrant.NewPointsSelectorFilter(f),
		Wait:           qdrant.PtrOf(true),
	}); err != nil {
		return fmt.Errorf("qdrant: delete: %w", err)
	}
	return nil
}

// repoPointsFilter is the repo scope of the GC (T140): the payload triple
// written by the worker (vcs_type, group_org=namespace, repo=short path).
// An empty vcs_type is omitted (a single-provider deployment still works).
func repoPointsFilter(vcsType, groupOrg, repo string) *qdrant.Filter {
	must := make([]*qdrant.Condition, 0, 3)
	if vcsType != "" {
		must = append(must, qdrant.NewMatch("vcs_type", vcsType))
	}
	must = append(must, qdrant.NewMatch("group_org", groupOrg), qdrant.NewMatch("repo", repo))
	return &qdrant.Filter{Must: must}
}

// CountRepoPoints returns the exact number of points of one repository — the
// read-back before an irreversible repo GC deletion (T140).
func (s *Store) CountRepoPoints(ctx context.Context, vcsType, groupOrg, repo string) (uint64, error) {
	return s.CountByFilter(ctx, repoPointsFilter(vcsType, groupOrg, repo))
}

// DeleteRepoPoints deletes all points of one repository (T140, Wait=true).
func (s *Store) DeleteRepoPoints(ctx context.Context, vcsType, groupOrg, repo string) error {
	return s.DeleteByFilter(ctx, repoPointsFilter(vcsType, groupOrg, repo))
}

// DeleteByIDs deletes points by their point IDs (T140 entity reconcile:
// IssueID/PullRequestID). The ID selector is used directly — no fallback
// filter is needed, so issue_id/pr_id stay out of the payload indexes.
func (s *Store) DeleteByIDs(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	pointIDs := make([]*qdrant.PointId, len(ids))
	for i, id := range ids {
		pointIDs[i] = qdrant.NewID(id)
	}
	if _, err := s.client.Delete(ctx, &qdrant.DeletePoints{
		CollectionName: s.collection,
		Points:         qdrant.NewPointsSelectorIDs(pointIDs),
		Wait:           qdrant.PtrOf(true),
	}); err != nil {
		return fmt.Errorf("qdrant: delete by ids: %w", err)
	}
	return nil
}

// DeleteCodeFile deletes all code points of a file on a branch (before
// overwriting the content with a new commit and when the file is deleted).
func (s *Store) DeleteCodeFile(ctx context.Context, repo, branch, filePath string) error {
	return s.DeleteByFilter(ctx, codeFilter(repo, branch, qdrant.NewMatch("file_path", filePath)))
}

// DeleteBranch deletes all code points of a vanished branch (data-model §2).
func (s *Store) DeleteBranch(ctx context.Context, repo, branch string) error {
	return s.DeleteByFilter(ctx, codeFilter(repo, branch))
}

// DeleteCodeCommits deletes code points of rewritten commits of a branch
// (prune after force-push, FR-016).
func (s *Store) DeleteCodeCommits(ctx context.Context, repo, branch string, shas []string) error {
	if len(shas) == 0 {
		return nil
	}
	return s.DeleteByFilter(ctx, codeFilter(repo, branch, qdrant.NewMatchKeywords("commit_sha", shas...)))
}

// DeleteCIFailure deletes a CI failure point (repo, pipeline_id, job_id) —
// prune after a job is restarted into success (data-model §6).
func (s *Store) DeleteCIFailure(ctx context.Context, repo, pipelineID, jobID string) error {
	return s.DeleteByFilter(ctx, &qdrant.Filter{Must: []*qdrant.Condition{
		qdrant.NewMatch("type", "ci_failure"),
		qdrant.NewMatch("repo", repo),
		qdrant.NewMatch("pipeline_id", pipelineID),
		qdrant.NewMatch("job_id", jobID),
	}})
}

// codeFilter builds must conditions for code points of a repository/branch.
func codeFilter(repo, branch string, extra ...*qdrant.Condition) *qdrant.Filter {
	must := []*qdrant.Condition{
		qdrant.NewMatch("type", "code"),
		qdrant.NewMatch("repo", repo),
		qdrant.NewMatch("branch", branch),
	}
	return &qdrant.Filter{Must: append(must, extra...)}
}
