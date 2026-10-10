package worker

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// Golden IDs match the values in internal/store/payload_test.go
// (an independent python check of the uuid5 formula) — mapping must not lose fields.

func TestMapIssue(t *testing.T) {
	it := mapIssue("github", "org", "myrepo", provider.Issue{
		ID: 7, Title: "Header", Body: "Body", State: "opened",
	})
	if it.ID != "79eb2957-ed8a-592d-99e5-4b0b5e1cda64" {
		t.Errorf("ID = %q", it.ID)
	}
	want := map[string]any{
		"vcs_type": "github", "group_org": "org", "repo": "myrepo", "type": "issue",
		"issue_id": int64(7), "state": "opened", "text": "Header\n\nBody",
	}
	if !reflect.DeepEqual(it.Payload, want) {
		t.Errorf("payload = %v, want %v", it.Payload, want)
	}
	if it.Text != "Header\n\nBody" {
		t.Errorf("text = %q", it.Text)
	}

	// empty body — title only
	bare := mapIssue("github", "org", "myrepo", provider.Issue{ID: 1, Title: "Only header"})
	if bare.Text != "Only header" {
		t.Errorf("text without body = %q", bare.Text)
	}
}

func TestMapChangeRequest(t *testing.T) {
	it := mapChangeRequest("gitlab", "grp", "myrepo", provider.ChangeRequest{
		ID: 42, Title: "PR header", Body: "PR body", State: "opened",
		SourceBranch: "feat", TargetBranch: "main",
	})
	if it.ID != "bd8787b5-9878-55f0-a044-7f60c392e290" {
		t.Errorf("ID = %q", it.ID)
	}
	want := map[string]any{
		"vcs_type": "gitlab", "group_org": "grp", "repo": "myrepo", "type": "pull_request",
		"pr_id": int64(42), "source_branch": "feat", "target_branch": "main",
		"state": "opened", "text": "PR header\n\nPR body",
	}
	if !reflect.DeepEqual(it.Payload, want) {
		t.Errorf("payload = %v, want %v", it.Payload, want)
	}
}

func TestMapCIFailureTail(t *testing.T) {
	var log strings.Builder
	for i := 1; i <= 100; i++ {
		fmt.Fprintf(&log, "line %d\n", i)
	}
	p := provider.Pipeline{ID: "100", Ref: "main", SHA: "abc"}
	j := provider.Job{ID: "1000", Name: "build", Status: "failed"}

	it := mapCIFailure("gitlab", "grp", "myrepo", p, j, log.String())
	if it.ID != "df747038-0516-562a-afc9-f7d6b1e2a8ff" {
		t.Errorf("ID = %q", it.ID)
	}
	for key, want := range map[string]any{
		"vcs_type": "gitlab", "group_org": "grp", "repo": "myrepo", "type": "ci_failure",
		"pipeline_id": "100", "job_id": "1000", "job_name": "build",
		"branch": "main", "commit_sha": "abc",
	} {
		if it.Payload[key] != want {
			t.Errorf("payload[%q] = %v, want %v", key, it.Payload[key], want)
		}
	}

	// text = title + ≤ ciTailLines log lines (the tail covers 51..100)
	lines := strings.Split(it.Text, "\n")
	if len(lines) > 1+ciTailLines {
		t.Errorf("lines in text %d, want ≤ %d", len(lines), 1+ciTailLines)
	}
	if !strings.Contains(lines[0], "build") || !strings.Contains(lines[0], "100") {
		t.Errorf("title = %q", lines[0])
	}
	if !strings.HasSuffix(it.Text, "line 100") {
		t.Error("the tail does not end with the last log line")
	}
	if strings.Contains(it.Text, "line 50") {
		t.Error("a line outside the last 50 got into the tail")
	}

	// empty log — title only
	bare := mapCIFailure("gitlab", "grp", "myrepo", p, j, "")
	if strings.Contains(bare.Text, "\n") {
		t.Errorf("text without log = %q", bare.Text)
	}
}

func TestJobFailed(t *testing.T) {
	if !jobFailed(provider.Job{Status: "failed"}) {
		t.Error("GitLab status=failed not recognized")
	}
	if !jobFailed(provider.Job{Conclusion: "failure"}) {
		t.Error("GitHub conclusion=failure not recognized")
	}
	if jobFailed(provider.Job{Status: "success", Conclusion: "success"}) {
		t.Error("success treated as a failure")
	}
}
