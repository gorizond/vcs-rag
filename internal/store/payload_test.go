package store

import (
	"reflect"
	"regexp"
	"testing"
)

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// Reference UUID5 values are computed independently (python uuid.uuid5,
// namespace = sha1("vcs-rag")[:16]) — a cross-check of the data-model §3–§6 formulas.

func TestCodeChunkID(t *testing.T) {
	const want = "7e107394-9035-5dab-aa21-e1540db10278"
	got := CodeChunkID("gitlab", "my-org/team-a/svc", "myrepo", "main", "abc123", "src/main.go", 0)
	if got != want {
		t.Errorf("CodeChunkID = %q, want %q", got, want)
	}
	if !uuidRe.MatchString(got) {
		t.Errorf("ID does not match the UUIDv5 format: %q", got)
	}
	if CodeChunkID("gitlab", "my-org/team-a/svc", "myrepo", "main", "abc123", "src/main.go", 1) == got {
		t.Error("chunk_index does not affect the ID")
	}
}

func TestPullRequestID(t *testing.T) {
	const want = "bd8787b5-9878-55f0-a044-7f60c392e290"
	if got := PullRequestID("gitlab", "myrepo", 42); got != want {
		t.Errorf("PullRequestID = %q, want %q", got, want)
	}
}

func TestIssueID(t *testing.T) {
	const want = "79eb2957-ed8a-592d-99e5-4b0b5e1cda64"
	if got := IssueID("github", "myrepo", 7); got != want {
		t.Errorf("IssueID = %q, want %q", got, want)
	}
}

func TestCIFailureID(t *testing.T) {
	const want = "df747038-0516-562a-afc9-f7d6b1e2a8ff"
	if got := CIFailureID("gitlab", "myrepo", "100", "1000"); got != want {
		t.Errorf("CIFailureID = %q, want %q", got, want)
	}
}

func TestCodePayload(t *testing.T) {
	got := CodePayload("gitlab", "grp", "repo", "main", "sha1", "a.go", "text", 2)
	want := map[string]any{
		"vcs_type": "gitlab", "group_org": "grp", "repo": "repo", "type": "code",
		"branch": "main", "commit_sha": "sha1", "file_path": "a.go", "text": "text",
		"chunk_index": 2,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CodePayload = %v, want %v", got, want)
	}
	for _, key := range []string{"vcs_type", "group_org", "repo", "type", "branch", "commit_sha", "file_path", "text", "chunk_index"} {
		if _, ok := got[key]; !ok {
			t.Errorf("payload has no required field %q", key)
		}
	}
}

func TestOtherPayloads(t *testing.T) {
	meta := EntityMeta{
		Author: "alice", Assignees: []string{"bob"}, Reviewers: []string{"carol"},
		Labels: []string{"bug"}, Milestone: "v1", State: "opened",
	}
	pr := PullRequestPayload("gitlab", "grp", "repo", "feat", "main", "text", 42, meta)
	wantPR := map[string]any{
		"vcs_type": "gitlab", "group_org": "grp", "repo": "repo", "type": "pull_request",
		"pr_id": int64(42), "source_branch": "feat", "target_branch": "main",
		"state": "opened", "text": "text",
		"author": "alice", "assignees": []string{"bob"}, "reviewers": []string{"carol"},
		"labels": []string{"bug"}, "milestone": "v1",
	}
	if !reflect.DeepEqual(pr, wantPR) {
		t.Errorf("PullRequestPayload = %v, want %v", pr, wantPR)
	}

	issue := IssuePayload("github", "org", "repo", "text", 7, EntityMeta{Author: "dave", Labels: []string{"p1"}, State: "closed"})
	wantIssue := map[string]any{
		"vcs_type": "github", "group_org": "org", "repo": "repo", "type": "issue",
		"issue_id": int64(7), "state": "closed", "text": "text",
		"author": "dave", "assignees": []string(nil), "reviewers": []string(nil),
		"labels": []string{"p1"}, "milestone": "",
	}
	if !reflect.DeepEqual(issue, wantIssue) {
		t.Errorf("IssuePayload = %v, want %v", issue, wantIssue)
	}

	// EntityMeta.Payload is the F3 subset handed to set_payload — every key is present.
	if p := (EntityMeta{State: "open"}).Payload(); len(p) != 6 || p["state"] != "open" || p["author"] != "" {
		t.Errorf("EntityMeta.Payload = %v", p)
	}

	ci := CIFailurePayload("gitlab", "grp", "repo", "100", "1000", "build", "main", "sha1", "log")
	wantCI := map[string]any{
		"vcs_type": "gitlab", "group_org": "grp", "repo": "repo", "type": "ci_failure",
		"pipeline_id": "100", "job_id": "1000", "job_name": "build",
		"branch": "main", "commit_sha": "sha1", "text": "log",
	}
	if !reflect.DeepEqual(ci, wantCI) {
		t.Errorf("CIFailurePayload = %v, want %v", ci, wantCI)
	}
}
