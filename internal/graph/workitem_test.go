package graph_test

import (
	"testing"
	"time"

	"github.com/qdrant/go-client/qdrant"

	"github.com/gorizond/vcs-rag/internal/graph"
	"github.com/gorizond/vcs-rag/internal/provider"
)

// TestWorkItemPointDeterministicAndPayload — T033/US3: the work-item writer
// keys the point on item type+id (a state/title edit rewrites the same node)
// and stamps updated_at only when the provider gave one (a zero datetime would
// index as year 1).
func TestWorkItemPointDeterministicAndPayload(t *testing.T) {
	meta := graph.Meta{Repo: "org/repo", GroupOrg: "org", VCSType: "gitlab", Branch: "main", SchemaVer: "1", Generation: 7}
	updated := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	item := provider.WorkItem{ID: 7, ItemType: "issue", State: "open", Title: "Fix it", UpdatedAt: updated}
	p := graph.WorkItemPoint(meta, item)
	if p.GetId().GetUuid() != graph.WorkItemPoint(meta, item).GetId().GetUuid() {
		t.Fatal("WorkItemPoint ID must be deterministic")
	}
	edited := item
	edited.State, edited.Title = "closed", "Fixed"
	if graph.WorkItemPoint(meta, edited).GetId().GetUuid() != p.GetId().GetUuid() {
		t.Fatal("WorkItemPoint ID must key on item type+id, not the mutable state/title")
	}
	// a task with the same numeric id is a distinct node.
	task := item
	task.ItemType = "task"
	if graph.WorkItemPoint(meta, task).GetId().GetUuid() == p.GetId().GetUuid() {
		t.Fatal("WorkItemPoint ID must separate item types")
	}

	m := p.GetPayload()
	for field, want := range map[string]string{
		"kind":       "work_item",
		"node_key":   graph.RefWorkItem(meta.Repo, "issue", "7"),
		"item_id":    "7",
		"item_type":  "issue",
		"state":      "open",
		"title":      "Fix it",
		"updated_at": "2026-02-03T04:05:06Z",
		"repo":       meta.Repo,
		"branch":     meta.Branch,
	} {
		if got := m[field].GetStringValue(); got != want {
			t.Errorf("WorkItemPoint payload[%s] = %q, want %q", field, got, want)
		}
	}
	if _, ok := graph.WorkItemPoint(meta, provider.WorkItem{ID: 7, ItemType: "issue"}).GetPayload()["updated_at"]; ok {
		t.Error("a zero updated_at must be omitted from the payload")
	}
}

// TestWorkItemRelationEdgeEndpoints — T033/US3: authored/assigned_to run
// work_item → user, carry the generic endpoint fields, and keep their ID
// across index generations (re-index idempotency).
func TestWorkItemRelationEdgeEndpoints(t *testing.T) {
	meta := graph.Meta{Repo: "org/repo", VCSType: "gitlab", Branch: "main", SchemaVer: "1", Generation: 7}
	item := provider.WorkItem{ID: 7, ItemType: "issue"}
	author, assignee := graph.RefUser("gitlab.com", "42"), graph.RefUser("gitlab.com", "43")
	itemKey := graph.RefWorkItem(meta.Repo, "issue", "7")
	for _, tc := range []struct {
		name string
		edge *qdrant.PointStruct
		rel  string
		user string
	}{
		{"authored", graph.AuthoredEdge(meta, item, author), "authored", author},
		{"assigned_to", graph.AssignedToEdge(meta, item, assignee), "assigned_to", assignee},
	} {
		m := tc.edge.GetPayload()
		for field, want := range map[string]string{
			"kind": "edge", "edge_type": tc.rel,
			"from_key": itemKey, "from_type": "work_item",
			"to_key": tc.user, "to_type": "user",
		} {
			if got := m[field].GetStringValue(); got != want {
				t.Errorf("%s payload[%s] = %q, want %q", tc.name, field, got, want)
			}
		}
	}
	if graph.AuthoredEdge(meta, item, author).GetId().GetUuid() == graph.AssignedToEdge(meta, item, author).GetId().GetUuid() {
		t.Fatal("authored and assigned_to to the same user must be distinct edges")
	}

	// another generation reuses every edge ID: re-indexing is idempotent.
	next := meta
	next.Generation++
	if graph.AuthoredEdge(next, item, author).GetId().GetUuid() != graph.AuthoredEdge(meta, item, author).GetId().GetUuid() {
		t.Fatal("edge IDs must stay stable across index generations")
	}
}
