package graph_test

import (
	"testing"

	"github.com/gorizond/vcs-rag/internal/graph"
	"github.com/gorizond/vcs-rag/internal/provider"
)

// TestUserPointDeterministicAndIdentified — T025/US2: the user writer keys the
// point on host+provider id (a login rename rewrites the same node) and carries
// the canonical node_key the authored/commented edges resolve to.
func TestUserPointDeterministicAndIdentified(t *testing.T) {
	meta := graph.Meta{Repo: "org/repo", GroupOrg: "org", VCSType: "gitlab", Branch: "main", SchemaVer: "1", Generation: 7}
	u := provider.User{ID: "42", Login: "alice", Name: "Alice", State: "active"}
	p := graph.UserPoint(meta, "gitlab.com", u)
	if p.GetId().GetUuid() != graph.UserPoint(meta, "gitlab.com", u).GetId().GetUuid() {
		t.Fatal("UserPoint ID must be deterministic")
	}
	renamed := u
	renamed.Login, renamed.Name = "alice2", "Alice Smith"
	if graph.UserPoint(meta, "gitlab.com", renamed).GetId().GetUuid() != p.GetId().GetUuid() {
		t.Fatal("UserPoint ID must key on host+id, not the mutable login")
	}
	if graph.UserPoint(meta, "github.com", u).GetId().GetUuid() == p.GetId().GetUuid() {
		t.Fatal("UserPoint ID must separate provider hosts")
	}

	m := p.GetPayload()
	for field, want := range map[string]string{
		"kind":        "user",
		"node_key":    graph.RefUser("gitlab.com", "42"),
		"vcs_user_id": "42",
		"login":       "alice",
		"name":        "Alice",
		"state":       "active",
		"repo":        meta.Repo,
		"branch":      meta.Branch,
		"schema_ver":  meta.SchemaVer,
	} {
		if got := m[field].GetStringValue(); got != want {
			t.Errorf("UserPoint payload[%s] = %q, want %q", field, got, want)
		}
	}
}

// TestAuthoredPointEndpoints — T025/US2: the authored edge (work_item → user)
// carries the generic endpoint fields (from_key/to_key/from_type/to_type) the
// neighbors/DSL traversal resolves.
func TestAuthoredPointEndpoints(t *testing.T) {
	meta := graph.Meta{Repo: "org/repo", VCSType: "gitlab", Branch: "main", SchemaVer: "1", Generation: 7}
	item, user := graph.RefWorkItem(meta.Repo, "issue", "5"), graph.RefUser("gitlab.com", "42")
	e := graph.AuthoredPoint(meta, item, user)
	if e.GetId().GetUuid() != graph.AuthoredPoint(meta, item, user).GetId().GetUuid() {
		t.Fatal("AuthoredPoint ID must be deterministic")
	}
	m := e.GetPayload()
	for field, want := range map[string]string{
		"kind": "edge", "edge_type": "authored",
		"from_key": item, "from_type": "work_item",
		"to_key": user, "to_type": "user",
		"repo": meta.Repo, "branch": meta.Branch,
	} {
		if got := m[field].GetStringValue(); got != want {
			t.Errorf("AuthoredPoint payload[%s] = %q, want %q", field, got, want)
		}
	}
}
