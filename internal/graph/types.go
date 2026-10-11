// Package graph provides pure structured-graph logic: entity types, canonical
// keys, and deterministic IDs. It must stay free of embedding/ML dependencies
// (invariant R3, enforced by graph_test.go).
package graph

import (
	"time"

	"github.com/qdrant/go-client/qdrant"
)

// Kind is the payload discriminator for graph entities in the vector store.
type Kind string

const (
	KindNode   Kind = "node"
	KindEdge   Kind = "edge"
	KindMrFile Kind = "mr_file"
	KindCi     Kind = "ci"
	KindVuln   Kind = "vuln"
)

// NodeType is the entity type carried by a graph node.
type NodeType string

const (
	NodeDefinition NodeType = "definition"
)

// Entity is the start-entity family of a graph query (DSL start.entity, R5).
type Entity string

const (
	EntityDefinition Entity = "definition"
	// EntityFile starts from a file's indexed content. The collection stores
	// no standalone file points: a file is represented by its definition
	// nodes (file_path/package/symbol) and its edges (from_file).
	EntityFile   Entity = "file"
	EntityMrFile Entity = "mr_file"
	EntityCi     Entity = "ci"
	EntityVuln   Entity = "vuln"
)

// EdgeType is the relation label carried by a graph edge.
type EdgeType string

const (
	EdgeImports EdgeType = "imports"
	EdgeDefines EdgeType = "defines"
)

// Direction selects the traversal direction relative to the start entity.
type Direction string

const (
	Outgoing Direction = "outgoing"
	Incoming Direction = "incoming"
)

// Node is a code-structure entity (definition) in a repository at a revision.
type Node struct {
	Symbol     string
	SymbolKind string
	Package    string
	FilePath   string
	Repo       string
	GroupOrg   string
	VCSType    string
	Branch     string
	CommitSHA  string
}

// Edge is a relation between two entities, valid for one index generation.
type Edge struct {
	EdgeType    EdgeType
	FromFile    string
	FromPackage string
	ToPackage   string
	ToSymbol    string
	External    bool
	Repo        string
	Branch      string
	Generation  int
}

// Coverage records how far a repository has been indexed.
type Coverage struct {
	Repo                   string
	IndexedCommit          string
	Generation             int
	IndexedAt              time.Time
	Truncated              bool
	LanguagesWithResolvers []string
}

// Filter narrows entities by attribute. Zero values match everything.
type Filter struct {
	Repo     string
	GroupOrg string
	VCSType  string
	Branch   string
	FilePath string
	Package  string
	Symbol   string
	// Conditions are extra raw Qdrant conditions (DSL operators:
	// match/in/range/is_null/...; see contracts/mcp-tools.md) ANDed with the
	// typed fields above. Field names are the indexed payload names (repo,
	// file_path, from_file, to_package, status, severity, ...). An empty
	// MatchAny in a must chain short-circuits the whole query (Compile sets
	// Compiled.MatchNone); oversized lists are chunked by the store.
	Conditions []*qdrant.Condition
}

// Query is one traversal request: start entity, edge selector, and bounds.
type Query struct {
	// StartEntity selects the start-node family; empty defaults to definition.
	StartEntity Entity
	Start       Filter
	Filter      Filter
	// EdgeType restricts expansion edges; empty or "*" means any edge type.
	EdgeType  EdgeType
	Direction Direction
	Hops      int
	Limit     int
}
