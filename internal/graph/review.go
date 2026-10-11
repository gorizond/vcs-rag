package graph

import (
	"strconv"
	"time"

	"github.com/qdrant/go-client/qdrant"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// Review family (schema v3; T022/T023, US2; data-model §new nodes, R2): the
// merge_request / merge_request_diff nodes and their relations. The builders
// are pure: no client, no I/O. Like every schema-v3 family, the deterministic
// NodeID/EdgeID input IS the canonical Ref* key (no opaque Key* helper), so a
// re-index rewrites the same point: generation stays out of the ID and in the
// payload.
//
// The in_project and fixes relations are shared with the security family:
// InProjectEdge/FixesEdge (security.go) are generic over the endpoint types,
// so review callers reuse them (fromType merge_request / merge_request_diff /
// merge_request_diff_file) instead of duplicating the builders.

// MergeRequestPoint builds the merge_request node (kind=merge_request, T022):
// iid is the provider request id (GitLab MR IID / GitHub PR number), the key
// part of RefMergeRequest, and doubles as the indexed iid column. author_key is
// the author's canonical user ref (RefUser) when the ingest lane resolved the
// provider username to a user id, "" otherwise — the node is still complete.
// The has_diff/has_latest_diff/has_file and *_by relations resolve to this
// node's node_key. updated_at is written only when the provider gave one
// (datetime index); created_at has no provider value yet. Pure: no client, no
// I/O.
func MergeRequestPoint(meta Meta, cr provider.ChangeRequest, authorKey string) *qdrant.PointStruct {
	iid := strconv.FormatInt(cr.ID, 10)
	key := RefMergeRequest(meta.Repo, iid)
	payload := map[string]any{
		"kind":             string(KindMergeRequest),
		"node_key":         key,
		"mr_key":           key, // the family's own ref, mirroring label_key
		"iid":              iid,
		"title":            cr.Title,
		"state":            cr.State,
		"author_key":       authorKey,
		"source_branch":    cr.SourceBranch,
		"target_branch":    cr.TargetBranch,
		"merged":           cr.Merged,
		"merge_commit_sha": cr.MergeCommitSHA,
		"repo":             meta.Repo,
		"group_org":        meta.GroupOrg,
		"vcs_type":         meta.VCSType,
		"branch":           meta.Branch,
		"schema_ver":       meta.SchemaVer,
		"generation":       meta.Generation,
		"indexed_at":       time.Now().UTC().Format(time.RFC3339),
	}
	if !cr.UpdatedAt.IsZero() {
		payload["updated_at"] = cr.UpdatedAt.UTC().Format(time.RFC3339)
	}
	return &qdrant.PointStruct{
		Id: qdrant.NewID(NodeID(Namespace(meta.SchemaVer), meta.Repo, meta.Branch,
			KindMergeRequest, key).String()),
		Payload: valueMap(payload),
	}
}

// MergeRequestDiffPoint builds the merge_request_diff node
// (kind=merge_request_diff, T022): one diff version (the provider diff_refs /
// versions triple). The key part is the head SHA, falling back to the version
// label (RefMergeRequestDiff = repo|mr:<iid>|diff:<head_sha|version>), so the
// tip and every stored version are distinct, stable nodes. mr_key links back
// to the request node; has_diff/has_latest_diff/has_file resolve to this
// node's node_key. Pure: no client, no I/O.
func MergeRequestDiffPoint(meta Meta, mrID int, d provider.MergeRequestDiffRef) *qdrant.PointStruct {
	iid := strconv.Itoa(mrID)
	ident := d.HeadSHA
	if ident == "" {
		ident = d.Version
	}
	key := RefMergeRequestDiff(meta.Repo, iid, ident)
	return &qdrant.PointStruct{
		Id: qdrant.NewID(NodeID(Namespace(meta.SchemaVer), meta.Repo, meta.Branch,
			KindMergeRequestDiff, key).String()),
		Payload: valueMap(map[string]any{
			"kind":       string(KindMergeRequestDiff),
			"node_key":   key,
			"mr_key":     RefMergeRequest(meta.Repo, iid),
			"version":    d.Version,
			"base_sha":   d.BaseSHA,
			"start_sha":  d.StartSHA,
			"head_sha":   d.HeadSHA,
			"state":      d.State,
			"repo":       meta.Repo,
			"group_org":  meta.GroupOrg,
			"vcs_type":   meta.VCSType,
			"branch":     meta.Branch,
			"schema_ver": meta.SchemaVer,
			"generation": meta.Generation,
			"indexed_at": time.Now().UTC().Format(time.RFC3339),
		}),
	}
}

// reviewEdge builds one review relation point: deterministic ID over the
// canonical endpoint keys (generation excluded) with the shared edge payload.
func reviewEdge(meta Meta, et EdgeType, fromKey, fromType, toKey, toType string) *qdrant.PointStruct {
	return &qdrant.PointStruct{
		Id: qdrant.NewID(EdgeID(Namespace(meta.SchemaVer), meta.Repo, meta.Branch,
			et, fromKey, toKey).String()),
		Payload: orbitEdgeMap(meta, et, fromKey, fromType, toKey, toType),
	}
}

// HasDiffEdge — merge_request → merge_request_diff (T023): every stored diff
// version of the request.
func HasDiffEdge(meta Meta, mrKey, diffKey string) *qdrant.PointStruct {
	return reviewEdge(meta, EdgeHasDiff, mrKey, "merge_request", diffKey, "merge_request_diff")
}

// HasLatestDiffEdge — merge_request → merge_request_diff (T023): the tip
// version of the request (the same pair as has_diff, restricted by the caller
// to the head diff).
func HasLatestDiffEdge(meta Meta, mrKey, diffKey string) *qdrant.PointStruct {
	return reviewEdge(meta, EdgeHasLatestDiff, mrKey, "merge_request", diffKey, "merge_request_diff")
}

// HasFileEdge — merge_request_diff → merge_request_diff_file (T023): fileKey
// is the diff-file point's canonical key. The legacy mr_file points have no
// node_key yet, so the ingest lane supplies the key it links them under.
func HasFileEdge(meta Meta, diffKey, fileKey string) *qdrant.PointStruct {
	return reviewEdge(meta, EdgeHasFile, diffKey, "merge_request_diff", fileKey, "merge_request_diff_file")
}

// MergedEdge — user → merge_request (T023): the user who merged the request.
func MergedEdge(meta Meta, userKey, mrKey string) *qdrant.PointStruct {
	return reviewEdge(meta, EdgeMerged, userKey, "user", mrKey, "merge_request")
}

// ReviewerEdge — user → merge_request (T023): one reviewer of the request.
func ReviewerEdge(meta Meta, userKey, mrKey string) *qdrant.PointStruct {
	return reviewEdge(meta, EdgeReviewer, userKey, "user", mrKey, "merge_request")
}

// MergedAtCommitEdge — merge_request → commit (T023). The commit node family
// and its Ref* key do not exist yet (ids.go), so nothing writes this edge
// today; the builder is exposed for the ingest lane once a commit key exists.
func MergedAtCommitEdge(meta Meta, mrKey, commitKey string) *qdrant.PointStruct {
	return reviewEdge(meta, EdgeMergedAtCommit, mrKey, "merge_request", commitKey, "commit")
}
