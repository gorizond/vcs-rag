package graph

import (
	"sort"

	"github.com/qdrant/go-client/qdrant"
)

// Imports family (schema v3; T029/T031, US3; data-model §new nodes, FR-023):
// the imported_symbol node and the symbol-level imports edges. Package-level
// imports (file→package, ImportPoints) stay exactly as they are; this family
// adds the symbol-level pair where it resolves — an unresolved import keeps its
// node with resolved=false and is not a gap. Builders are pure: no client, no
// I/O; like every schema-v3 family the NodeID/EdgeID inputs are the canonical
// Ref* keys.

// ImportedSymbolPoint builds the imported_symbol node (kind=imported_symbol,
// T029). name is the imported package's bare name (PackageName), the spelling
// definition nodes carry in `package`; resolved marks whether a definition of
// the same repository matched it (FR-023), target_definition_key then carrying
// that definition's node_key (RefFile of its file) — the to_key of the linking
// imported_symbol→definition edge. Pure: no client, no I/O.
func ImportedSymbolPoint(meta Meta, file, name string, resolved bool, targetDefKey string) *qdrant.PointStruct {
	key := RefImportedSymbol(meta.Repo, file, name)
	return &qdrant.PointStruct{
		Id: qdrant.NewID(NodeID(Namespace(meta.SchemaVer), meta.Repo, meta.Branch,
			KindImportedSymbol, key).String()),
		Payload: structureNodePayload(meta, KindImportedSymbol, key, map[string]any{
			"name":                  name,
			"file_path":             file,
			"resolved":              resolved,
			"target_definition_key": targetDefKey,
		}),
	}
}

// ImportsEdge — the imports relation (T031): file→imported_symbol for every
// parsed import, imported_symbol→definition only when the import resolved
// (resolved=true). fromType/toType name the endpoint families, since the one
// relation spans both pairs (same pattern as HasFindingEdge).
func ImportsEdge(meta Meta, fromKey, fromType, toKey, toType string) *qdrant.PointStruct {
	return structureEdge(meta, EdgeImports, fromKey, fromType, toKey, toType)
}

// ImportedSymbolPoints derives the imported_symbol family of one scan pass
// (T033/US3, FR-023) from the parsed imports: one node per (file, imported
// package name) pair plus a file→imported_symbol imports edge. An import
// resolves when it is not external (isExternal; nil means nothing is external,
// matching ImportPoints) and a definition of the same repository carries its
// bare package name (PackageName — the spelling definition nodes are keyed by);
// the resolved node links imported_symbol→definition with the definition
// file's node_key (RefFile) as the target. Several declaring files pick the
// lexicographically smallest one, so a repo re-scan rewrites the same target.
// Output is sorted by file path and deduped per file+name, so repeated passes
// build byte-identical point sets. Pure: no client, no I/O.
func ImportedSymbolPoints(meta Meta, files []FileGraph, isExternal func(string) bool) []*qdrant.PointStruct {
	defFile := make(map[string]string)
	for _, f := range files {
		for _, d := range f.Defs {
			if d.Package == "" || d.FilePath == "" {
				continue
			}
			if cur, ok := defFile[d.Package]; !ok || d.FilePath < cur {
				defFile[d.Package] = d.FilePath
			}
		}
	}
	ordered := make([]FileGraph, len(files))
	copy(ordered, files)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Path < ordered[j].Path })

	var out []*qdrant.PointStruct
	for _, f := range ordered {
		if f.Path == "" {
			continue
		}
		from := RefFile(meta.Repo, f.Path)
		seen := make(map[string]bool, len(f.Imports))
		for _, imp := range f.Imports {
			name := PackageName(imp.Path)
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			key := RefImportedSymbol(meta.Repo, f.Path, name)
			resolved, target := false, ""
			if isExternal == nil || !isExternal(imp.Path) {
				if file, ok := defFile[name]; ok {
					resolved, target = true, RefFile(meta.Repo, file)
				}
			}
			out = append(out, ImportedSymbolPoint(meta, f.Path, name, resolved, target))
			out = append(out, ImportsEdge(meta, from, "file", key, "imported_symbol"))
			if resolved {
				out = append(out, ImportsEdge(meta, key, "imported_symbol", target, "definition"))
			}
		}
	}
	return out
}
