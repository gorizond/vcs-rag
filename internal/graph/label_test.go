package graph_test

import (
	"testing"

	"github.com/gorizond/vcs-rag/internal/graph"
	"github.com/gorizond/vcs-rag/internal/provider"
)

// TestLabelPointDeterministicAndScoped — T033/US3: a label's identity is
// scope+name (no provider id), carried as both node_key and label_key; the
// same name in another scope is another node.
func TestLabelPointDeterministicAndScoped(t *testing.T) {
	meta := graph.Meta{Repo: "org/repo", GroupOrg: "org", VCSType: "gitlab", Branch: "main", SchemaVer: "1", Generation: 7}
	l := provider.Label{Name: "bug", Scope: "project", Color: "#d73a4a"}
	p := graph.LabelPoint(meta, l)
	if p.GetId().GetUuid() != graph.LabelPoint(meta, l).GetId().GetUuid() {
		t.Fatal("LabelPoint ID must be deterministic")
	}
	if graph.LabelPoint(meta, provider.Label{Name: "bug"}).GetId().GetUuid() == p.GetId().GetUuid() {
		t.Fatal("LabelPoint ID must key on scope+name: the scope is part of the identity")
	}

	ref := graph.RefLabel("project", "bug")
	m := p.GetPayload()
	for field, want := range map[string]string{
		"kind": "label", "node_key": ref, "label_key": ref,
		"name": "bug", "scope": "project",
		"repo": meta.Repo, "branch": meta.Branch,
	} {
		if got := m[field].GetStringValue(); got != want {
			t.Errorf("LabelPoint payload[%s] = %q, want %q", field, got, want)
		}
	}
}

// TestLabelledEdgeShape — T033/US3: labelled runs work_item → label, keyed by
// scope+name exactly as the node is.
func TestLabelledEdgeShape(t *testing.T) {
	meta := graph.Meta{Repo: "org/repo", VCSType: "gitlab", Branch: "main", SchemaVer: "1", Generation: 7}
	item := provider.WorkItem{ID: 7, ItemType: "issue"}
	e := graph.LabelledEdge(meta, item, "project", "bug")
	if e.GetId().GetUuid() != graph.LabelledEdge(meta, item, "project", "bug").GetId().GetUuid() {
		t.Fatal("LabelledEdge ID must be deterministic")
	}
	m := e.GetPayload()
	for field, want := range map[string]string{
		"kind": "edge", "edge_type": "labelled",
		"from_key": graph.RefWorkItem(meta.Repo, "issue", "7"), "from_type": "work_item",
		"to_key": graph.RefLabel("project", "bug"), "to_type": "label",
	} {
		if got := m[field].GetStringValue(); got != want {
			t.Errorf("LabelledEdge payload[%s] = %q, want %q", field, got, want)
		}
	}
}
