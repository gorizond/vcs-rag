package graph

import (
	"strconv"
	"strings"
	"time"

	"github.com/qdrant/go-client/qdrant"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// MaxNoteBodyBytes caps the stored body text of one note point (2 KiB): the body
// is a payload blob for reading a comment, never embedded (R3, plan §4).
const MaxNoteBodyBytes = 2 << 10

// clipNoteBody cuts body to MaxNoteBodyBytes without splitting a UTF-8 rune.
func clipNoteBody(body string) string {
	if len(body) <= MaxNoteBodyBytes {
		return body
	}
	return strings.ToValidUTF8(body[:MaxNoteBodyBytes], "")
}

// NotePoint builds the note/comment node of one issue or change request
// (kind=note, R6/US2). parentKey and authorKey are the canonical node_key refs
// of the commented work item (RefWorkItem) and the author (RefUser); the builder
// takes them as arguments so the spelling is shared with the work-item/user
// writers. body is capped at MaxNoteBodyBytes and never indexed; the zero
// created_at/updated_at are omitted. Pure: no client, no I/O.
func NotePoint(meta Meta, n provider.Note, parentKey, authorKey string) *qdrant.PointStruct {
	id := strconv.FormatInt(n.ID, 10)
	payload := map[string]any{
		"kind":         string(KindNote),
		"node_key":     RefNote(meta.Repo, id),
		"note_id":      id,
		"parent_key":   parentKey,
		"author_key":   authorKey,
		"system":       n.System,
		"body":         clipNoteBody(n.Body),
		"last_seen_at": time.Now().UTC().Format(time.RFC3339),
		"repo":         meta.Repo,
		"group_org":    meta.GroupOrg,
		"vcs_type":     meta.VCSType,
		"branch":       meta.Branch,
		"schema_ver":   meta.SchemaVer,
		"generation":   meta.Generation,
		"indexed_at":   time.Now().UTC().Format(time.RFC3339),
	}
	if v := rfc3339(n.CreatedAt); v != "" {
		payload["created_at"] = v
	}
	if v := rfc3339(n.UpdatedAt); v != "" {
		payload["updated_at"] = v
	}
	return &qdrant.PointStruct{
		Id: qdrant.NewID(NodeID(Namespace(meta.SchemaVer), meta.Repo, meta.Branch,
			KindNote, KeyNote(id)).String()),
		Payload: valueMap(payload),
	}
}

// CommentedPoint builds the commented edge (note → user, R7/US2); noteKey and
// userKey are the canonical node_key refs of the note and its author. Pure.
func CommentedPoint(meta Meta, noteKey, userKey string) *qdrant.PointStruct {
	return &qdrant.PointStruct{
		Id: qdrant.NewID(EdgeID(Namespace(meta.SchemaVer), meta.Repo, meta.Branch,
			EdgeCommented, noteKey, userKey).String()),
		Payload: orbitEdgeMap(meta, EdgeCommented, noteKey, "note", userKey, "user"),
	}
}

// rfc3339 formats a datetime-indexed payload field (created_at/updated_at); the
// zero time is omitted — Qdrant would not index the empty string anyway.
func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
