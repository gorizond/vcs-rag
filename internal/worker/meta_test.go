package worker

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// Эталонные ID совпадают с golden-значениями internal/store/payload_test.go
// (независимая python-проверка формулы uuid5) — маппинг не должен терять поля.

func TestMapIssue(t *testing.T) {
	it := mapIssue("github", "org", "myrepo", provider.Issue{
		ID: 7, Title: "Заголовок", Body: "Тело", State: "opened",
	})
	if it.ID != "79eb2957-ed8a-592d-99e5-4b0b5e1cda64" {
		t.Errorf("ID = %q", it.ID)
	}
	want := map[string]any{
		"vcs_type": "github", "group_org": "org", "repo": "myrepo", "type": "issue",
		"issue_id": int64(7), "state": "opened", "text": "Заголовок\n\nТело",
	}
	if !reflect.DeepEqual(it.Payload, want) {
		t.Errorf("payload = %v, want %v", it.Payload, want)
	}
	if it.Text != "Заголовок\n\nТело" {
		t.Errorf("text = %q", it.Text)
	}

	// тело пустое — только заголовок
	bare := mapIssue("github", "org", "myrepo", provider.Issue{ID: 1, Title: "Только заголовок"})
	if bare.Text != "Только заголовок" {
		t.Errorf("text без тела = %q", bare.Text)
	}
}

func TestMapChangeRequest(t *testing.T) {
	it := mapChangeRequest("gitlab", "grp", "myrepo", provider.ChangeRequest{
		ID: 42, Title: "PR заголовок", Body: "PR тело", State: "opened",
		SourceBranch: "feat", TargetBranch: "main",
	})
	if it.ID != "bd8787b5-9878-55f0-a044-7f60c392e290" {
		t.Errorf("ID = %q", it.ID)
	}
	want := map[string]any{
		"vcs_type": "gitlab", "group_org": "grp", "repo": "myrepo", "type": "pull_request",
		"pr_id": int64(42), "source_branch": "feat", "target_branch": "main",
		"state": "opened", "text": "PR заголовок\n\nPR тело",
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

	// text = заголовок + ≤ ciTailLines строк лога (в хвост входят 51..100)
	lines := strings.Split(it.Text, "\n")
	if len(lines) > 1+ciTailLines {
		t.Errorf("строк в text %d, ожидалось ≤ %d", len(lines), 1+ciTailLines)
	}
	if !strings.Contains(lines[0], "build") || !strings.Contains(lines[0], "100") {
		t.Errorf("заголовок = %q", lines[0])
	}
	if !strings.HasSuffix(it.Text, "line 100") {
		t.Error("хвост не заканчивается последней строкой лога")
	}
	if strings.Contains(it.Text, "line 50") {
		t.Error("в хвост попала строка вне последних 50")
	}

	// пустой лог — только заголовок
	bare := mapCIFailure("gitlab", "grp", "myrepo", p, j, "")
	if strings.Contains(bare.Text, "\n") {
		t.Errorf("text без лога = %q", bare.Text)
	}
}

func TestJobFailed(t *testing.T) {
	if !jobFailed(provider.Job{Status: "failed"}) {
		t.Error("GitLab status=failed не распознан")
	}
	if !jobFailed(provider.Job{Conclusion: "failure"}) {
		t.Error("GitHub conclusion=failure не распознан")
	}
	if jobFailed(provider.Job{Status: "success", Conclusion: "success"}) {
		t.Error("success принят за падение")
	}
}
