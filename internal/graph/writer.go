package graph

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/qdrant/go-client/qdrant"

	"github.com/gorizond/vcs-rag/internal/chunk"
)

// Meta is the per-repo scan context stamped on every graph point of one pass.
// SchemaVer selects the ID namespace (Namespace), Generation is the sweep
// generation of the pass (T011), CommitSHA the revision the tree was read at.
type Meta struct {
	Repo       string
	GroupOrg   string
	VCSType    string
	Branch     string
	CommitSHA  string
	Language   string
	SchemaVer  string
	Generation int
}

// FileGraph is one scanned file: its repo-relative path, Go package, and the
// definitions/imports extracted from it (chunk.Extract, T012).
type FileGraph struct {
	Path    string
	Package string
	Defs    []chunk.GraphDefinition
	Imports []chunk.GraphImport
}

// GraphWriter is the write surface WriteGraph needs; *store.GraphStore
// satisfies it. The interface keeps the writer testable without Qdrant (same
// pattern as the worker's graphStore).
type GraphWriter interface {
	Upsert(ctx context.Context, points []*qdrant.PointStruct) error
}

// symbolName is the canonical symbol name of a definition: methods carry their
// receiver, so a method never collides with a same-named function or with a
// method on another type (data-model canonical key: def:<pkg>.<name>[#recv]).
func symbolName(d chunk.GraphDefinition) string {
	if d.Receiver == "" {
		return d.Name
	}
	return d.Name + "#" + d.Receiver
}

// DefinitionPoints builds the definition nodes (kind=node,
// node_type=definition) of one file's extracted definitions. Pure: no client,
// no I/O.
func DefinitionPoints(meta Meta, defs []chunk.GraphDefinition) []*qdrant.PointStruct {
	ns := Namespace(meta.SchemaVer)
	out := make([]*qdrant.PointStruct, 0, len(defs))
	for _, d := range defs {
		sym := symbolName(d)
		out = append(out, &qdrant.PointStruct{
			Id: qdrant.NewID(NodeID(ns, meta.Repo, meta.Branch, KindNode, KeyDef(d.Package, sym)).String()),
			Payload: valueMap(map[string]any{
				"kind":        string(KindNode),
				"node_type":   string(NodeDefinition),
				"symbol":      sym,
				"symbol_text": sym, // text index (search_definitions match.text)
				"symbol_kind": d.Kind,
				"package":     d.Package,
				"file_path":   d.FilePath,
				"signature":   d.Signature,
				"commit_sha":  meta.CommitSHA,
				"repo":        meta.Repo,
				"group_org":   meta.GroupOrg,
				"vcs_type":    meta.VCSType,
				"branch":      meta.Branch,
				"language":    meta.Language,
				"schema_ver":  meta.SchemaVer,
				"generation":  meta.Generation,
				"indexed_at":  time.Now().UTC().Format(time.RFC3339),
			}),
		})
	}
	return out
}

// DefinePoints builds the defines edges (kind=edge, edge_type=defines) from a
// file to each symbol it declares. The symbol spelling matches
// DefinitionPoints exactly, so to_symbol joins back to the node. Pure.
func DefinePoints(meta Meta, defs []chunk.GraphDefinition) []*qdrant.PointStruct {
	ns := Namespace(meta.SchemaVer)
	out := make([]*qdrant.PointStruct, 0, len(defs))
	for _, d := range defs {
		sym := symbolName(d)
		out = append(out, &qdrant.PointStruct{
			Id: qdrant.NewID(EdgeID(ns, meta.Repo, meta.Branch, EdgeDefines, KeyFile(d.FilePath), KeyDefine(d.FilePath, sym)).String()),
			Payload: valueMap(map[string]any{
				"kind":       string(KindEdge),
				"edge_type":  string(EdgeDefines),
				"from_file":  d.FilePath,
				"to_symbol":  sym,
				"repo":       meta.Repo,
				"branch":     meta.Branch,
				"generation": meta.Generation,
				"indexed_at": time.Now().UTC().Format(time.RFC3339),
			}),
		})
	}
	return out
}

// ImportPoints builds the package-level imports edges (kind=edge,
// edge_type=imports) of one file. from_package/to_package are repo-namespaced
// (PkgKey) so same-named packages ("utils", "main") never collide across
// repositories. external marks imports outside the indexed repository set and
// is classified by the caller: nil means nothing is external. Pure: no client,
// no I/O.
func ImportPoints(meta Meta, f FileGraph, isExternal func(string) bool) []*qdrant.PointStruct {
	ns := Namespace(meta.SchemaVer)
	from := PkgKey(meta.Repo, f.Package)
	out := make([]*qdrant.PointStruct, 0, len(f.Imports))
	for _, imp := range f.Imports {
		to := PkgKey(meta.Repo, imp.Path)
		out = append(out, &qdrant.PointStruct{
			Id: qdrant.NewID(EdgeID(ns, meta.Repo, meta.Branch, EdgeImports, KeyFile(f.Path), KeyImport(f.Path, to)).String()),
			Payload: valueMap(map[string]any{
				"kind":         string(KindEdge),
				"edge_type":    string(EdgeImports),
				"from_file":    f.Path,
				"from_package": from,
				"to_package":   to,
				"external":     isExternal != nil && isExternal(imp.Path),
				"repo":         meta.Repo,
				"branch":       meta.Branch,
				"generation":   meta.Generation,
				"indexed_at":   time.Now().UTC().Format(time.RFC3339),
			}),
		})
	}
	return out
}

// KindCoverage is the payload discriminator of the per-repo coverage marker
// (T057). It is deliberately absent from Sweep's kind list (gc.go): coverage
// is rewritten in place on every completed scan, never removed by generation.
const KindCoverage Kind = "coverage"

// CoveragePoint builds the per-repo coverage marker (T057): how far the graph
// scan of one repository has got. The ID is deterministic (NodeID over the
// KeyCoverage canonical key) — a completed pass upserts the same point again,
// last writer wins. Pure: no client, no I/O.
func CoveragePoint(meta Meta, indexedCommit string) *qdrant.PointStruct {
	payload := valueMap(map[string]any{
		"kind":           string(KindCoverage),
		"repo":           meta.Repo,
		"branch":         meta.Branch,
		"indexed_commit": indexedCommit,
		"generation":     meta.Generation,
		"schema_ver":     meta.SchemaVer,
		"language":       meta.Language,
		"indexed_at":     time.Now().UTC().Format(time.RFC3339),
	})
	// The resolver list is the one non-scalar payload field; valueMap is
	// scalar-only by design. Go is the only language with a resolver today.
	payload["languages_with_resolvers"] = qdrant.NewValueFromList(qdrant.NewValueString("go"))
	return &qdrant.PointStruct{
		Id:      qdrant.NewID(NodeID(Namespace(meta.SchemaVer), meta.Repo, meta.Branch, KindCoverage, KeyCoverage(meta.Repo)).String()),
		Payload: payload,
	}
}

// WriteGraph builds every node and edge of one repo+branch scan pass and
// upserts them through gs (batched inside GraphStore.Upsert). It returns the
// number of points written.
func WriteGraph(ctx context.Context, gs GraphWriter, files []FileGraph, meta Meta, isExternal func(string) bool) (int, error) {
	var points []*qdrant.PointStruct
	for _, f := range files {
		points = append(points, DefinitionPoints(meta, f.Defs)...)
		points = append(points, DefinePoints(meta, f.Defs)...)
		points = append(points, ImportPoints(meta, f, isExternal)...)
	}
	if err := gs.Upsert(ctx, points); err != nil {
		return 0, err
	}
	return len(points), nil
}

// valueMap converts a flat payload into Qdrant values. Strings are made valid
// UTF-8 here (T158) before Qdrant sees them: NewValueMap panics on invalid
// bytes, and repo paths/names come from git, not from Go source. Only the
// scalar kinds the graph payloads use are accepted; a nested value is a code
// bug, not input.
func valueMap(m map[string]any) map[string]*qdrant.Value {
	out := make(map[string]*qdrant.Value, len(m))
	for k, v := range m {
		switch t := v.(type) {
		case string:
			out[k] = qdrant.NewValueString(strings.ToValidUTF8(t, "\uFFFD"))
		case bool:
			out[k] = qdrant.NewValueBool(t)
		case int:
			out[k] = qdrant.NewValueInt(int64(t))
		default:
			panic(fmt.Sprintf("graph: payload %q has unsupported type %T", k, v))
		}
	}
	return out
}
