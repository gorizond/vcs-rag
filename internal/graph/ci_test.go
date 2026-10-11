package graph_test

import (
	"testing"

	"github.com/gorizond/vcs-rag/internal/graph"
	"github.com/gorizond/vcs-rag/internal/provider"
)

// TestCiVulnPoints covers the US4/US6 writers (T043/T047): deterministic
// repo-scoped IDs, the payload contract, and the shared CI status vocabulary.
func TestCiVulnPoints(t *testing.T) {
	meta := graph.Meta{
		Repo: "org/repo", GroupOrg: "org", VCSType: "gitlab", Branch: "main",
		CommitSHA: "abc", SchemaVer: "1", Generation: 7,
	}
	ok := graph.CiPoint(meta, graph.CiLevelJob, "100", "build", "test", "success", 1500, "main", "", "abc")
	same := graph.CiPoint(meta, graph.CiLevelJob, "100", "build", "test", "success", 1500, "main", "", "abc")
	if ok.GetId().GetUuid() != same.GetId().GetUuid() {
		t.Fatal("CiPoint ID must be deterministic")
	}
	failed := graph.CiPoint(meta, graph.CiLevelJob, "100", "build", "test", "failed", 1500, "main", "", "abc")
	if failed.GetId().GetUuid() != ok.GetId().GetUuid() {
		t.Fatal("CiPoint ID must key on pipeline+job, not status (in-place status update)")
	}
	if got := failed.GetPayload()["status"].GetStringValue(); got != "failed" {
		t.Fatalf("failed status = %q", got)
	}
	if got := ok.GetPayload()["ci_level"].GetStringValue(); got != "job" {
		t.Fatalf("ci_level = %q", got)
	}
	if got := ok.GetPayload()["duration_ms"].GetIntegerValue(); got != 1500 {
		t.Fatalf("duration_ms = %d", got)
	}
	if got := ok.GetPayload()["branch"].GetStringValue(); got != "main" {
		t.Fatalf("branch = %q (the sweep filter needs it)", got)
	}

	v := graph.VulnPoint(meta, "42", "high", "dependabot", "detected", "go.mod")
	if v.GetId().GetUuid() != graph.VulnPoint(meta, "42", "high", "dependabot", "detected", "go.mod").GetId().GetUuid() {
		t.Fatal("VulnPoint ID must be deterministic")
	}
	if got := v.GetPayload()["location_file"].GetStringValue(); got != "go.mod" {
		t.Fatalf("location_file = %q", got)
	}

	for _, tc := range []struct{ status, conclusion, want string }{
		{"success", "", "success"},
		{"failed", "", "failed"},
		{"running", "", "running"},
		{"manual", "", "pending"},
		{"completed", "success", "success"},
		{"completed", "failure", "failed"},
		{"completed", "timed_out", "failed"},
		{"completed", "cancelled", "canceled"},
		{"completed", "skipped", "skipped"},
		{"in_progress", "", "running"},
		{"queued", "", "pending"},
	} {
		if got := provider.NormalizeCIStatus(tc.status, tc.conclusion); got != tc.want {
			t.Errorf("NormalizeCIStatus(%q,%q) = %q, want %q", tc.status, tc.conclusion, got, tc.want)
		}
	}
}
