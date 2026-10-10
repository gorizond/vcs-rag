package store

import "fmt"

// Сборка ID и payload'ов точек — формулы из data-model.md §3–§6.

// CodeChunkID — детерминированный ID точки кода:
// uuid5(ns, "code|{vcs_type}|{group_org}|{repo}|{branch}|{commit_sha}|{file_path}|{chunk_index}").
func CodeChunkID(vcsType, groupOrg, repo, branch, commitSHA, filePath string, chunkIndex int) string {
	return uuid5(uuidNamespace, fmt.Sprintf("code|%s|%s|%s|%s|%s|%s|%d",
		vcsType, groupOrg, repo, branch, commitSHA, filePath, chunkIndex))
}

// CodePayload — payload точки type=code (data-model.md §3).
func CodePayload(vcsType, groupOrg, repo, branch, commitSHA, filePath, text string, chunkIndex int) map[string]any {
	return map[string]any{
		"vcs_type":    vcsType,
		"group_org":   groupOrg,
		"repo":        repo,
		"type":        "code",
		"branch":      branch,
		"commit_sha":  commitSHA,
		"file_path":   filePath,
		"text":        text,
		"chunk_index": chunkIndex,
	}
}

// PullRequestID — ID точки MR/PR: uuid5(ns, "pr|{vcs_type}|{repo}|{pr_id}").
func PullRequestID(vcsType, repo string, prID int64) string {
	return uuid5(uuidNamespace, fmt.Sprintf("pr|%s|%s|%d", vcsType, repo, prID))
}

// PullRequestPayload — payload точки type=pull_request (data-model.md §4).
func PullRequestPayload(vcsType, groupOrg, repo, sourceBranch, targetBranch, state, text string, prID int64) map[string]any {
	return map[string]any{
		"vcs_type":      vcsType,
		"group_org":     groupOrg,
		"repo":          repo,
		"type":          "pull_request",
		"pr_id":         prID,
		"source_branch": sourceBranch,
		"target_branch": targetBranch,
		"state":         state,
		"text":          text,
	}
}

// IssueID — ID точки issue: uuid5(ns, "issue|{vcs_type}|{repo}|{issue_id}").
func IssueID(vcsType, repo string, issueID int64) string {
	return uuid5(uuidNamespace, fmt.Sprintf("issue|%s|%s|%d", vcsType, repo, issueID))
}

// IssuePayload — payload точки type=issue (data-model.md §5).
func IssuePayload(vcsType, groupOrg, repo, state, text string, issueID int64) map[string]any {
	return map[string]any{
		"vcs_type":  vcsType,
		"group_org": groupOrg,
		"repo":      repo,
		"type":      "issue",
		"issue_id":  issueID,
		"state":     state,
		"text":      text,
	}
}

// CIFailureID — ID точки CI-фейла: uuid5(ns, "ci|{vcs_type}|{repo}|{pipeline_id}|{job_id}").
func CIFailureID(vcsType, repo, pipelineID, jobID string) string {
	return uuid5(uuidNamespace, fmt.Sprintf("ci|%s|%s|%s|%s", vcsType, repo, pipelineID, jobID))
}

// CIFailurePayload — payload точки type=ci_failure (data-model.md §6).
func CIFailurePayload(vcsType, groupOrg, repo, pipelineID, jobID, jobName, branch, commitSHA, text string) map[string]any {
	return map[string]any{
		"vcs_type":    vcsType,
		"group_org":   groupOrg,
		"repo":        repo,
		"type":        "ci_failure",
		"pipeline_id": pipelineID,
		"job_id":      jobID,
		"job_name":    jobName,
		"branch":      branch,
		"commit_sha":  commitSHA,
		"text":        text,
	}
}
