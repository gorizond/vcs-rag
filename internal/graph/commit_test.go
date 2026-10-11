package graph_test

import (
	"testing"

	"github.com/gorizond/vcs-rag/internal/graph"
)

// TestCommitNodeAndMergedAtCommit — T011/US1: the commit node keys on
// repo|commit:<sha> with kind=commit, the merged_at_commit edge runs
// merge_request → commit over those canonical keys, and the node ID is
// generation-free (a later pass of the same revision rewrites the same point).
func TestCommitNodeAndMergedAtCommit(t *testing.T) {
	const sha = "c0ffee1234567890"
	meta := graph.Meta{Repo: "org/repo", GroupOrg: "org", VCSType: "gitlab", Branch: "main", SchemaVer: "4", Generation: 7}
	c := graph.CommitPoint(meta, meta.Repo, sha)

	wantKey := graph.RefCommit(meta.Repo, sha)
	if wantKey != "org/repo|commit:"+sha {
		t.Fatalf("RefCommit = %q, want the repo|commit:<sha> form", wantKey)
	}
	for field, want := range map[string]string{
		"kind": "commit", "node_key": wantKey, "sha": sha,
		"repo": meta.Repo, "branch": meta.Branch, "schema_ver": meta.SchemaVer,
	} {
		if got := pointField(c, field); got != want {
			t.Errorf("CommitPoint payload[%s] = %q, want %q", field, got, want)
		}
	}

	mrKey := graph.RefMergeRequest(meta.Repo, "42")
	e := graph.MergedAtCommitEdge(meta, mrKey, wantKey)
	for field, want := range map[string]string{
		"edge_type": "merged_at_commit",
		"from_key":  mrKey, "from_type": "merge_request",
		"to_key": wantKey, "to_type": "commit",
	} {
		if got := pointField(e, field); got != want {
			t.Errorf("MergedAtCommitEdge payload[%s] = %q, want %q", field, got, want)
		}
	}

	next := meta
	next.Generation = meta.Generation + 1
	if graph.CommitPoint(next, next.Repo, sha).GetId().GetUuid() != c.GetId().GetUuid() {
		t.Error("CommitPoint ID must not depend on generation")
	}
	if graph.CommitPoint(meta, meta.Repo, "deadbeef").GetId().GetUuid() == c.GetId().GetUuid() {
		t.Error("CommitPoint ID must separate shas")
	}
}
