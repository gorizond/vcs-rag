package graph

import (
	"strconv"
	"strings"
	"time"

	"github.com/qdrant/go-client/qdrant"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// MrFile — per-file diff of a change request (kind=mr_file, F2/US2). The patch
// is a payload blob by design: it is capped and never indexed (R2).
//
// MaxPatchBytes caps the stored diff text of one mr_file point (64 KiB): a
// whole-file patch of a generated or minified file can be megabytes; the graph
// payload is for reading a diff, not for archiving it.
const MaxPatchBytes = 64 << 10

// clipPatch cuts the patch to MaxPatchBytes without splitting a UTF-8 rune and
// reports whether it was cut.
func clipPatch(patch string) (string, bool) {
	if len(patch) <= MaxPatchBytes {
		return patch, false
	}
	return strings.ToValidUTF8(patch[:MaxPatchBytes], ""), true
}

// MrFilePoint builds the mr_file point of one changed file of a change request
// (payload-only, deterministic ID: re-writing the same (repo, branch, mr, file)
// keeps its ID). truncated is set when the provider flagged the diff (too
// large/collapsed) or the stored patch was cut to MaxPatchBytes. file_path is
// the new path of the file — renames join on the current path (OldPath is the
// fallback for deleted entries without one). node_key exposes the canonical
// RefMergeRequestDiffFile form the has_file edge resolves against (F13). Pure:
// no client, no I/O.
func MrFilePoint(meta Meta, mrID int, file provider.ChangeRequestFile, author, state string) *qdrant.PointStruct {
	filePath := file.NewPath
	if filePath == "" {
		filePath = file.OldPath
	}
	patch, cut := clipPatch(file.Patch)
	return &qdrant.PointStruct{
		Id: qdrant.NewID(NodeID(Namespace(meta.SchemaVer), meta.Repo, meta.Branch,
			KindMrFile, KeyMrFile(strconv.Itoa(mrID), filePath)).String()),
		Payload: valueMap(map[string]any{
			"kind":            string(KindMrFile),
			"mr_id":           mrID,
			"file_path":       filePath,
			"node_key":        RefMergeRequestDiffFile(meta.Repo, mrID, filePath),
			"status":          file.Status,
			"author":          author,
			"state":           state,
			"patch":           patch,
			"patch_truncated": file.Truncated || cut,
			"repo":            meta.Repo,
			"group_org":       meta.GroupOrg,
			"vcs_type":        meta.VCSType,
			"branch":          meta.Branch,
			"schema_ver":      meta.SchemaVer,
			"generation":      meta.Generation,
			"indexed_at":      time.Now().UTC().Format(time.RFC3339),
		}),
	}
}
