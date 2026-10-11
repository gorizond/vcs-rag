package graph_test

import (
	"testing"
	"time"

	"github.com/gorizond/vcs-rag/internal/graph"
	"github.com/gorizond/vcs-rag/internal/provider"
)

// TestMilestonePointDeterministicAndDueDate — T033/US3: the milestone writer
// keys the node on its provider id and carries due_date only when the provider
// gave one (an absent pointer is not a zero datetime).
func TestMilestonePointDeterministicAndDueDate(t *testing.T) {
	meta := graph.Meta{Repo: "org/repo", GroupOrg: "org", VCSType: "gitlab", Branch: "main", SchemaVer: "1", Generation: 7}
	due := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	ms := provider.Milestone{ID: 8, Title: "v2", State: "active", DueDate: &due}
	p := graph.MilestonePoint(meta, ms)
	if p.GetId().GetUuid() != graph.MilestonePoint(meta, ms).GetId().GetUuid() {
		t.Fatal("MilestonePoint ID must be deterministic")
	}

	m := p.GetPayload()
	for field, want := range map[string]string{
		"kind":     "milestone",
		"node_key": graph.RefMilestone(meta.Repo, "8"),
		"ms_id":    "8",
		"title":    "v2",
		"state":    "active",
		"due_date": "2026-03-01T00:00:00Z",
		"repo":     meta.Repo,
		"branch":   meta.Branch,
	} {
		if got := m[field].GetStringValue(); got != want {
			t.Errorf("MilestonePoint payload[%s] = %q, want %q", field, got, want)
		}
	}
	if _, ok := graph.MilestonePoint(meta, provider.Milestone{ID: 8, Title: "v2"}).GetPayload()["due_date"]; ok {
		t.Error("a nil due date must be omitted from the payload")
	}
}

// TestInMilestoneEdgeShape — T033/US3: in_milestone runs work_item → milestone;
// the target key is RefMilestone(repo,id) exactly as MilestonePoint keys it.
func TestInMilestoneEdgeShape(t *testing.T) {
	meta := graph.Meta{Repo: "org/repo", VCSType: "gitlab", Branch: "main", SchemaVer: "1", Generation: 7}
	item := provider.WorkItem{ID: 7, ItemType: "issue"}
	e := graph.InMilestoneEdge(meta, item, "8")
	if e.GetId().GetUuid() != graph.InMilestoneEdge(meta, item, "8").GetId().GetUuid() {
		t.Fatal("InMilestoneEdge ID must be deterministic")
	}
	m := e.GetPayload()
	for field, want := range map[string]string{
		"kind": "edge", "edge_type": "in_milestone",
		"from_key": graph.RefWorkItem(meta.Repo, "issue", "7"), "from_type": "work_item",
		"to_key": graph.RefMilestone(meta.Repo, "8"), "to_type": "milestone",
	} {
		if got := m[field].GetStringValue(); got != want {
			t.Errorf("InMilestoneEdge payload[%s] = %q, want %q", field, got, want)
		}
	}
}
