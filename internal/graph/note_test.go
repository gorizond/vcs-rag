package graph_test

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gorizond/vcs-rag/internal/graph"
	"github.com/gorizond/vcs-rag/internal/provider"
)

// TestNotePointCappedBodyAndTimestamps — T025/US2: the note writer keys the
// point on the note id (an edit rewrites the same point), stores the body
// capped at MaxNoteBodyBytes without splitting a rune, and omits zero
// timestamps (Qdrant would index them as year 1).
func TestNotePointCappedBodyAndTimestamps(t *testing.T) {
	meta := graph.Meta{Repo: "org/repo", GroupOrg: "org", VCSType: "gitlab", Branch: "main", SchemaVer: "1", Generation: 7}
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	n := provider.Note{ID: 7, ParentKind: "issue", ParentID: 5, AuthorID: "42",
		Body: "hello", CreatedAt: created, UpdatedAt: created.Add(time.Hour)}
	parent, author := graph.RefWorkItem(meta.Repo, "issue", "5"), graph.RefUser("gitlab.com", "42")
	p := graph.NotePoint(meta, n, parent, author)
	if p.GetId().GetUuid() != graph.NotePoint(meta, n, parent, author).GetId().GetUuid() {
		t.Fatal("NotePoint ID must be deterministic")
	}
	edited := n
	edited.Body = "hello, edited"
	if graph.NotePoint(meta, edited, parent, author).GetId().GetUuid() != p.GetId().GetUuid() {
		t.Fatal("NotePoint ID must key on the note id: an edit rewrites the same point")
	}

	m := p.GetPayload()
	for field, want := range map[string]string{
		"kind": "note", "node_key": graph.RefNote(meta.Repo, "7"), "note_id": "7",
		"parent_key": parent, "author_key": author, "body": "hello",
		"created_at": "2026-01-02T03:04:05Z", "updated_at": "2026-01-02T04:04:05Z",
		"repo": meta.Repo, "branch": meta.Branch,
	} {
		if got := m[field].GetStringValue(); got != want {
			t.Errorf("NotePoint payload[%s] = %q, want %q", field, got, want)
		}
	}
	if _, ok := m["last_seen_at"]; !ok {
		t.Error("NotePoint payload lacks last_seen_at")
	}

	// zero timestamps are omitted, not indexed as the zero time.
	zm := graph.NotePoint(meta, provider.Note{ID: 8, Body: "x"}, parent, "").GetPayload()
	for _, field := range []string{"created_at", "updated_at"} {
		if _, ok := zm[field]; ok {
			t.Errorf("zero %s must be omitted from the payload", field)
		}
	}

	// a body over the cap is cut on a rune boundary, never split mid-rune.
	long := strings.Repeat("a", graph.MaxNoteBodyBytes-1) + strings.Repeat("é", 10)
	body := graph.NotePoint(meta, provider.Note{ID: 9, Body: long}, parent, author).GetPayload()["body"].GetStringValue()
	if len(body) > graph.MaxNoteBodyBytes || !utf8.ValidString(body) {
		t.Errorf("capped body: %d bytes, valid=%v (cap %d)", len(body), utf8.ValidString(body), graph.MaxNoteBodyBytes)
	}
	if body == long {
		t.Error("a body over the cap must be clipped")
	}
	// a body exactly at the cap is untouched.
	exact := strings.Repeat("b", graph.MaxNoteBodyBytes)
	if got := graph.NotePoint(meta, provider.Note{ID: 10, Body: exact}, parent, author).GetPayload()["body"].GetStringValue(); got != exact {
		t.Errorf("a body at the cap was altered: %d bytes", len(got))
	}
}

// TestCommentedPointEndpoints — T025/US2: the commented edge (note → user)
// carries the generic endpoint fields the DSL traverses.
func TestCommentedPointEndpoints(t *testing.T) {
	meta := graph.Meta{Repo: "org/repo", VCSType: "gitlab", Branch: "main", SchemaVer: "1", Generation: 7}
	note, user := graph.RefNote(meta.Repo, "7"), graph.RefUser("gitlab.com", "42")
	e := graph.CommentedPoint(meta, note, user)
	if e.GetId().GetUuid() != graph.CommentedPoint(meta, note, user).GetId().GetUuid() {
		t.Fatal("CommentedPoint ID must be deterministic")
	}
	m := e.GetPayload()
	for field, want := range map[string]string{
		"kind": "edge", "edge_type": "commented",
		"from_key": note, "from_type": "note",
		"to_key": user, "to_type": "user",
	} {
		if got := m[field].GetStringValue(); got != want {
			t.Errorf("CommentedPoint payload[%s] = %q, want %q", field, got, want)
		}
	}
}
