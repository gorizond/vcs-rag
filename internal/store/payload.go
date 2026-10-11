package store

import "fmt"

// Point ID and payload builders — formulas from data-model.md §3–§6.

// CodeChunkID — deterministic code point ID:
// uuid5(ns, "code|{vcs_type}|{group_org}|{repo}|{branch}|{commit_sha}|{file_path}|{chunk_index}").
func CodeChunkID(vcsType, groupOrg, repo, branch, commitSHA, filePath string, chunkIndex int) string {
	return uuid5(uuidNamespace, fmt.Sprintf("code|%s|%s|%s|%s|%s|%s|%d",
		vcsType, groupOrg, repo, branch, commitSHA, filePath, chunkIndex))
}

// CodePayload — payload of a type=code point (data-model.md §3).
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

// PullRequestID — MR/PR point ID: uuid5(ns, "pr|{vcs_type}|{repo}|{pr_id}").
func PullRequestID(vcsType, repo string, prID int64) string {
	return uuid5(uuidNamespace, fmt.Sprintf("pr|%s|%s|%d", vcsType, repo, prID))
}

// EntityMeta — the additive issue/PR payload fields (data-model §7, F3):
// author/assignees/reviewers/labels/milestone and the canonical state. They
// ride along in the upsert payload of new/changed points and are written alone
// via SetPayload for existing points — never a re-upsert (LM budget).
type EntityMeta struct {
	Author    string
	Assignees []string
	Reviewers []string
	Labels    []string
	Milestone string
	State     string
}

// Payload returns the F3 fields as a payload map (the SetPayload subset).
func (m EntityMeta) Payload() map[string]any {
	return map[string]any{
		"author":    m.Author,
		"assignees": m.Assignees,
		"reviewers": m.Reviewers,
		"labels":    m.Labels,
		"milestone": m.Milestone,
		"state":     m.State,
	}
}

// PullRequestPayload — payload of a type=pull_request point (data-model.md §4,
// F3 fields from meta).
func PullRequestPayload(vcsType, groupOrg, repo, sourceBranch, targetBranch, text string, prID int64, meta EntityMeta) map[string]any {
	p := map[string]any{
		"vcs_type":      vcsType,
		"group_org":     groupOrg,
		"repo":          repo,
		"type":          "pull_request",
		"pr_id":         prID,
		"source_branch": sourceBranch,
		"target_branch": targetBranch,
		"text":          text,
	}
	return entityPayload(p, meta)
}

// IssueID — issue point ID: uuid5(ns, "issue|{vcs_type}|{repo}|{issue_id}").
func IssueID(vcsType, repo string, issueID int64) string {
	return uuid5(uuidNamespace, fmt.Sprintf("issue|%s|%s|%d", vcsType, repo, issueID))
}

// IssuePayload — payload of a type=issue point (data-model.md §5, F3 fields
// from meta).
func IssuePayload(vcsType, groupOrg, repo, text string, issueID int64, meta EntityMeta) map[string]any {
	p := map[string]any{
		"vcs_type":  vcsType,
		"group_org": groupOrg,
		"repo":      repo,
		"type":      "issue",
		"issue_id":  issueID,
		"text":      text,
	}
	return entityPayload(p, meta)
}

// entityPayload merges the F3 fields into a base issue/PR payload.
func entityPayload(base map[string]any, meta EntityMeta) map[string]any {
	for k, v := range meta.Payload() {
		base[k] = v
	}
	return base
}

// CIFailureID — CI failure point ID: uuid5(ns, "ci|{vcs_type}|{repo}|{pipeline_id}|{job_id}").
func CIFailureID(vcsType, repo, pipelineID, jobID string) string {
	return uuid5(uuidNamespace, fmt.Sprintf("ci|%s|%s|%s|%s", vcsType, repo, pipelineID, jobID))
}

// CIFailurePayload — payload of a type=ci_failure point (data-model.md §6).
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
