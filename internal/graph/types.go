// Package graph provides pure structured-graph logic: entity types, canonical
// keys, and deterministic IDs. It must stay free of embedding/ML dependencies
// (invariant R3, enforced by graph_test.go).
package graph

import "time"

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
}

// Query is one traversal request: start entity, edge selector, and bounds.
type Query struct {
	Start     Filter
	Filter    Filter
	EdgeType  EdgeType
	Direction Direction
	Hops      int
	Limit     int
}
