package graph

import (
	"time"

	"github.com/qdrant/go-client/qdrant"
)

// F13 literal-parity commit family (schema v4; T009/US1; data-model §New node,
// research R2): the commit node, emitted from the merge-request listing of one
// repository — Orbit derives Commit from the MR too (merged_commit_id), there
// is no separate commit reader. Like every schema-v3+ family the deterministic
// NodeID input IS the canonical Ref* key (no opaque Key* helper), so a
// re-index rewrites the same point: generation stays out of the ID and in the
// payload.
//
// The node carries only the sha the provider exposes: the full metadata
// (message/author/date) has no provider source and stays out. The kind is
// gated by SweepTargets.Review in the sweep (gc.go), as the MR listing is its
// only source. Pure: no client, no I/O.

// CommitPoint builds the commit node (kind=commit, T009): sha is the provider
// merge commit (ChangeRequest.MergeCommitSHA) and the key part of
// RefCommit(repo, sha) = repo|commit:<sha>. The node is the target of the
// merged_at_commit edge (review.go). Pure: no client, no I/O.
func CommitPoint(meta Meta, repo, sha string) *qdrant.PointStruct {
	key := RefCommit(repo, sha)
	return &qdrant.PointStruct{
		Id: qdrant.NewID(NodeID(Namespace(meta.SchemaVer), repo, meta.Branch,
			KindCommit, key).String()),
		Payload: valueMap(map[string]any{
			"kind":       string(KindCommit),
			"node_key":   key,
			"sha":        sha,
			"repo":       repo,
			"group_org":  meta.GroupOrg,
			"vcs_type":   meta.VCSType,
			"branch":     meta.Branch,
			"schema_ver": meta.SchemaVer,
			"generation": meta.Generation,
			"indexed_at": time.Now().UTC().Format(time.RFC3339),
		}),
	}
}
