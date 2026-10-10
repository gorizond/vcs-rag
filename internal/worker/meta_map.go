package worker

import (
	"fmt"

	"github.com/gorizond/vcs-rag/internal/provider"
	"github.com/gorizond/vcs-rag/internal/store"
)

// ciTailLines — хвост лога CI-джоба, попадающий в текст точки (data-model §6).
const ciTailLines = 50

// metaItem — готовая точка метаданных: ID, payload и текст для эмбеддинга.
type metaItem struct {
	ID      string
	Payload map[string]any
	Text    string
}

// mapIssue — issue → точка type=issue (data-model §5).
func mapIssue(vcsType, groupOrg, repo string, iss provider.Issue) metaItem {
	text := titleBody(iss.Title, iss.Body)
	return metaItem{
		ID:      store.IssueID(vcsType, repo, iss.ID),
		Payload: store.IssuePayload(vcsType, groupOrg, repo, iss.State, text, iss.ID),
		Text:    text,
	}
}

// mapChangeRequest — MR/PR → точка type=pull_request (data-model §4).
func mapChangeRequest(vcsType, groupOrg, repo string, cr provider.ChangeRequest) metaItem {
	text := titleBody(cr.Title, cr.Body)
	return metaItem{
		ID: store.PullRequestID(vcsType, repo, cr.ID),
		Payload: store.PullRequestPayload(vcsType, groupOrg, repo,
			cr.SourceBranch, cr.TargetBranch, cr.State, text, cr.ID),
		Text: text,
	}
}

// mapCIFailure — упавший job → точка type=ci_failure (data-model §6):
// text = заголовок + последние ≤ ciTailLines строк лога.
func mapCIFailure(vcsType, groupOrg, repo string, p provider.Pipeline, j provider.Job, logTail string) metaItem {
	text := ciText(p, j, logTail)
	return metaItem{
		ID:      store.CIFailureID(vcsType, repo, p.ID, j.ID),
		Payload: store.CIFailurePayload(vcsType, groupOrg, repo, p.ID, j.ID, j.Name, p.Ref, p.SHA, text),
		Text:    text,
	}
}

// titleBody собирает текст «заголовок + тело».
func titleBody(title, body string) string {
	if body == "" {
		return title
	}
	return title + "\n\n" + body
}

// ciText собирает текст CI-фейла: заголовок + хвост лога (≤50 строк).
func ciText(p provider.Pipeline, j provider.Job, logTail string) string {
	title := fmt.Sprintf("CI failure: job %s failed (pipeline %s, ref %s)", j.Name, p.ID, p.Ref)
	tail := provider.TailLines(logTail, ciTailLines)
	if tail == "" {
		return title
	}
	return title + "\n" + tail
}

// jobFailed — падение job'а: GitLab (status) или GitHub (conclusion).
func jobFailed(j provider.Job) bool {
	return j.Status == "failed" || j.Conclusion == "failure"
}
