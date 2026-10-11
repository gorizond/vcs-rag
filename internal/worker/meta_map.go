package worker

import (
	"fmt"

	"github.com/gorizond/vcs-rag/internal/provider"
	"github.com/gorizond/vcs-rag/internal/store"
)

// ciTailLines is the CI job log tail included in the point text (data-model §6).
const ciTailLines = 50

// metaItem is a ready metadata point: ID, payload and text for embedding.
type metaItem struct {
	ID      string
	Payload map[string]any // full upsert payload (includes the F3 fields)
	// MetaPayload — the F3 subset (author/assignees/reviewers/labels/milestone/
	// state), written via set_payload to points that are NOT re-embedded (T029).
	MetaPayload map[string]any
	Text        string
}

// mapIssue maps an issue to a type=issue point (data-model §5).
func mapIssue(vcsType, groupOrg, repo string, iss provider.Issue) metaItem {
	text := titleBody(iss.Title, iss.Body)
	meta := store.EntityMeta{
		Author: iss.Author, Assignees: iss.Assignees, Labels: iss.Labels,
		Milestone: iss.Milestone, State: iss.State,
	}
	return metaItem{
		ID:          store.IssueID(vcsType, repo, iss.ID),
		Payload:     store.IssuePayload(vcsType, groupOrg, repo, text, iss.ID, meta),
		MetaPayload: meta.Payload(),
		Text:        text,
	}
}

// mapChangeRequest maps an MR/PR to a type=pull_request point (data-model §4).
func mapChangeRequest(vcsType, groupOrg, repo string, cr provider.ChangeRequest) metaItem {
	text := titleBody(cr.Title, cr.Body)
	meta := store.EntityMeta{
		Author: cr.Author, Assignees: cr.Assignees, Reviewers: cr.Reviewers,
		Labels: cr.Labels, Milestone: cr.Milestone, State: cr.State,
	}
	return metaItem{
		ID:          store.PullRequestID(vcsType, repo, cr.ID),
		Payload:     store.PullRequestPayload(vcsType, groupOrg, repo, cr.SourceBranch, cr.TargetBranch, text, cr.ID, meta),
		MetaPayload: meta.Payload(),
		Text:        text,
	}
}

// mapCIFailure maps a failed job to a type=ci_failure point (data-model §6):
// text = title + the last ≤ ciTailLines log lines.
func mapCIFailure(vcsType, groupOrg, repo string, p provider.Pipeline, j provider.Job, logTail string) metaItem {
	text := ciText(p, j, logTail)
	return metaItem{
		ID:      store.CIFailureID(vcsType, repo, p.ID, j.ID),
		Payload: store.CIFailurePayload(vcsType, groupOrg, repo, p.ID, j.ID, j.Name, p.Ref, p.SHA, text),
		Text:    text,
	}
}

// titleBody builds the "title + body" text.
func titleBody(title, body string) string {
	if body == "" {
		return title
	}
	return title + "\n\n" + body
}

// ciText builds the CI failure text: title + log tail (≤50 lines).
func ciText(p provider.Pipeline, j provider.Job, logTail string) string {
	title := fmt.Sprintf("CI failure: job %s failed (pipeline %s, ref %s)", j.Name, p.ID, p.Ref)
	tail := provider.TailLines(logTail, ciTailLines)
	if tail == "" {
		return title
	}
	return title + "\n" + tail
}

// jobFailed reports a job failure: GitLab (status) or GitHub (conclusion).
func jobFailed(j provider.Job) bool {
	return j.Status == "failed" || j.Conclusion == "failure"
}
