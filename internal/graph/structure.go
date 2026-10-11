package graph

import (
	"path"
	"sort"
	"time"

	"github.com/qdrant/go-client/qdrant"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// Structure family (schema v3; T028/T030, US3; data-model §new nodes): the
// file/directory/branch nodes and their contains/on_branch relations. The
// builders are pure: no client, no I/O. Like every schema-v3 family the
// deterministic NodeID/EdgeID input IS the canonical Ref* key (no opaque Key*
// helper), so a re-index rewrites the same point: generation stays out of the
// ID and in the payload.
//
// Branch nodes are written by the provider tree ingest (T034); this family
// owns their builder and the code-graph halves (file/directory) of the tree.

// structureNodePayload is the shared payload of the structure and
// imported_symbol node builders (structure.go/imports.go): the kind
// discriminator, the canonical node_key, the family columns, and the scope
// every graph point stamps.
func structureNodePayload(meta Meta, kind Kind, nodeKey string, fields map[string]any) map[string]*qdrant.Value {
	m := map[string]any{
		"kind":       string(kind),
		"node_key":   nodeKey,
		"repo":       meta.Repo,
		"group_org":  meta.GroupOrg,
		"vcs_type":   meta.VCSType,
		"branch":     meta.Branch,
		"schema_ver": meta.SchemaVer,
		"generation": meta.Generation,
		"indexed_at": time.Now().UTC().Format(time.RFC3339),
	}
	for k, v := range fields {
		m[k] = v
	}
	return valueMap(m)
}

// dirOf normalizes a file's directory: filepath semantics with the repository
// root spelled "" instead of "." (the RefDirectory key part, data-model §new
// nodes).
func dirOf(p string) string {
	if d := path.Dir(p); d != "." {
		return d
	}
	return ""
}

// FilePoint builds the file node (kind=file, T028): one repository file of the
// code graph. node_key is RefFile — the same ref the file's definition nodes
// carry (R1) — so contains/on_branch/imports endpoints resolve to it. dir_key
// is the containing directory's ref (root: ""), name the base name, language
// the pass language. Pure: no client, no I/O.
func FilePoint(meta Meta, filePath string) *qdrant.PointStruct {
	key := RefFile(meta.Repo, filePath)
	return &qdrant.PointStruct{
		Id: qdrant.NewID(NodeID(Namespace(meta.SchemaVer), meta.Repo, meta.Branch,
			KindFile, key).String()),
		Payload: structureNodePayload(meta, KindFile, key, map[string]any{
			"file_path": filePath,
			"name":      path.Base(filePath),
			"dir_key":   RefDirectory(meta.Repo, dirOf(filePath)),
			"language":  meta.Language,
		}),
	}
}

// DirectoryPoint builds the directory node (kind=directory, T028): one tree
// directory, the repository root spelled "" (root=true) with the project name
// as its display name. node_key is RefDirectory; dir_key mirrors it so the
// family's own ref is filterable, like mr_key on merge_request. Pure: no
// client, no I/O.
func DirectoryPoint(meta Meta, dirPath string) *qdrant.PointStruct {
	key := RefDirectory(meta.Repo, dirPath)
	name := path.Base(dirPath)
	if dirPath == "" {
		name = path.Base(meta.Repo)
	}
	return &qdrant.PointStruct{
		Id: qdrant.NewID(NodeID(Namespace(meta.SchemaVer), meta.Repo, meta.Branch,
			KindDirectory, key).String()),
		Payload: structureNodePayload(meta, KindDirectory, key, map[string]any{
			"path":    dirPath,
			"name":    name,
			"root":    dirPath == "",
			"dir_key": key,
		}),
	}
}

// BranchPoint builds the branch node (kind=branch, T028) from the provider's
// branch listing (T034): name is the key part of RefBranch and doubles as the
// indexed column; default/protected are the provider flags. Pure: no client,
// no I/O.
func BranchPoint(meta Meta, b provider.Branch) *qdrant.PointStruct {
	key := RefBranch(meta.Repo, b.Name)
	return &qdrant.PointStruct{
		Id: qdrant.NewID(NodeID(Namespace(meta.SchemaVer), meta.Repo, meta.Branch,
			KindBranch, key).String()),
		Payload: structureNodePayload(meta, KindBranch, key, map[string]any{
			"name":      b.Name,
			"default":   b.Default,
			"protected": b.Protected,
		}),
	}
}

// structureEdge builds one structure relation point: deterministic ID over the
// canonical endpoint keys (generation excluded) with the shared edge payload.
func structureEdge(meta Meta, et EdgeType, fromKey, fromType, toKey, toType string) *qdrant.PointStruct {
	return &qdrant.PointStruct{
		Id: qdrant.NewID(EdgeID(Namespace(meta.SchemaVer), meta.Repo, meta.Branch,
			et, fromKey, toKey).String()),
		Payload: orbitEdgeMap(meta, et, fromKey, fromType, toKey, toType),
	}
}

// ContainsEdge — the tree relation contains (T030): branch→directory,
// directory→directory, directory→file. fromType/toType name the endpoint
// families, since the one relation spans three pairs (same pattern as
// HasFindingEdge).
func ContainsEdge(meta Meta, fromKey, fromType, toKey, toType string) *qdrant.PointStruct {
	return structureEdge(meta, EdgeContains, fromKey, fromType, toKey, toType)
}

// OnBranchEdge — the tree relation on_branch (T030): file→branch,
// directory→branch. branchKey is the branch node's canonical ref (RefBranch);
// the branch node itself is written by the provider tree ingest (T034).
// fromType is the source family (file|directory).
func OnBranchEdge(meta Meta, fromKey, fromType, branchKey string) *qdrant.PointStruct {
	return structureEdge(meta, EdgeOnBranch, fromKey, fromType, branchKey, "branch")
}

// StructurePoints derives the structure family of one scan pass (T033/US3) from
// the scanned files: one file node per file (the union of the FileGraph paths
// and the definition file_paths), one directory node per unique directory
// (including the root ""), the contains tree (dir→dir, dir→file) and the
// on_branch edges (file|directory→branch). Branch nodes are NOT emitted here:
// they come from the provider tree ingest (T034), the edges reference the
// branch the pass ran on. Output is sorted by path and duplicate-free, so
// repeated passes build byte-identical point sets. Pure: no client, no I/O.
func StructurePoints(meta Meta, files []FileGraph) []*qdrant.PointStruct {
	fileSet := make(map[string]bool)
	for _, f := range files {
		if f.Path != "" {
			fileSet[f.Path] = true
		}
		for _, d := range f.Defs {
			if d.FilePath != "" {
				fileSet[d.FilePath] = true
			}
		}
	}
	paths := make([]string, 0, len(fileSet))
	for p := range fileSet {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	dirSet := make(map[string]bool, len(paths))
	for _, p := range paths {
		for d := dirOf(p); !dirSet[d]; d = dirOf(d) {
			dirSet[d] = true
			if d == "" {
				break
			}
		}
	}
	dirs := make([]string, 0, len(dirSet))
	for d := range dirSet {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)

	branchKey := RefBranch(meta.Repo, meta.Branch)
	out := make([]*qdrant.PointStruct, 0, 3*(len(dirs)+len(paths)))
	for _, d := range dirs {
		out = append(out, DirectoryPoint(meta, d))
		if d != "" {
			out = append(out, ContainsEdge(meta,
				RefDirectory(meta.Repo, dirOf(d)), "directory",
				RefDirectory(meta.Repo, d), "directory"))
		}
		out = append(out, OnBranchEdge(meta, RefDirectory(meta.Repo, d), "directory", branchKey))
	}
	for _, p := range paths {
		out = append(out, FilePoint(meta, p))
		out = append(out, ContainsEdge(meta,
			RefDirectory(meta.Repo, dirOf(p)), "directory",
			RefFile(meta.Repo, p), "file"))
		out = append(out, OnBranchEdge(meta, RefFile(meta.Repo, p), "file", branchKey))
	}
	return out
}
