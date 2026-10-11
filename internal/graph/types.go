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
	// Orbit-parity node families (plan R6): user/note/work_item/milestone/
	// label/runner. They are payload discriminators, like KindCi/KindVuln.
	KindUser      Kind = "user"
	KindNote      Kind = "note"
	KindWorkItem  Kind = "work_item"
	KindMilestone Kind = "milestone"
	KindLabel     Kind = "label"
	KindRunner    Kind = "runner"
	// Orbit-parity gap catalog (schema v3): structure, review and security
	// node families. mr_file/vuln stay as accepted aliases of
	// merge_request_diff_file/vulnerability.
	KindGroup                   Kind = "group"
	KindProject                 Kind = "project"
	KindBranch                  Kind = "branch"
	KindDirectory               Kind = "directory"
	KindFile                    Kind = "file"
	KindImportedSymbol          Kind = "imported_symbol"
	KindMergeRequest            Kind = "merge_request"
	KindMergeRequestDiff        Kind = "merge_request_diff"
	KindMergeRequestDiffFile    Kind = "merge_request_diff_file"
	KindVulnerability           Kind = "vulnerability"
	KindSecurityScan            Kind = "security_scan"
	KindFinding                 Kind = "finding"
	KindVulnerabilityOccurrence Kind = "vulnerability_occurrence"
	KindVulnerabilityIdentifier Kind = "vulnerability_identifier"
	KindVulnerabilityScanner    Kind = "vulnerability_scanner"
)

// KindAlias returns the legacy spelling of a canonical v3 node kind and
// whether the family has one (data-model aliases, R6): mr_file ↔
// merge_request_diff_file, vuln ↔ vulnerability. The legacy spelling keeps its
// exact pre-v3 match (FR-022); the canonical spelling is resolved to both, so
// traversal works before and after a reindex (FR-019).
func KindAlias(k Kind) (Kind, bool) {
	switch k {
	case KindMergeRequestDiffFile:
		return KindMrFile, true
	case KindVulnerability:
		return KindVuln, true
	}
	return "", false
}

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
	// Orbit-parity gap entities (schema v3). merge_request_diff_file and
	// vulnerability keep their legacy aliases mr_file and vuln.
	EntityGroup                   Entity = "group"
	EntityProject                 Entity = "project"
	EntityBranch                  Entity = "branch"
	EntityDirectory               Entity = "directory"
	EntityImportedSymbol          Entity = "imported_symbol"
	EntityMergeRequest            Entity = "merge_request"
	EntityMergeRequestDiff        Entity = "merge_request_diff"
	EntityMergeRequestDiffFile    Entity = "merge_request_diff_file"
	EntityVulnerability           Entity = "vulnerability"
	EntitySecurityScan            Entity = "security_scan"
	EntityFinding                 Entity = "finding"
	EntityVulnerabilityOccurrence Entity = "vulnerability_occurrence"
	EntityVulnerabilityIdentifier Entity = "vulnerability_identifier"
	EntityVulnerabilityScanner    Entity = "vulnerability_scanner"
)

// EdgeType is the relation label carried by a graph edge.
type EdgeType string

const (
	EdgeImports EdgeType = "imports"
	EdgeDefines EdgeType = "defines"
	// Orbit-parity relations (plan R7): work-item/note/user/label/milestone/
	// runner links that the file/package-specific types cannot express.
	EdgeAuthored    EdgeType = "authored"     // work_item → user
	EdgeCommented   EdgeType = "commented"    // note → user
	EdgeAssignedTo  EdgeType = "assigned_to"  // work_item → user
	EdgeLabelled    EdgeType = "labelled"     // work_item → label
	EdgeInMilestone EdgeType = "in_milestone" // work_item → milestone
	EdgeExecutedBy  EdgeType = "executed_by"  // ci → runner
	// Orbit-parity gap relations (schema v3).
	EdgeContains       EdgeType = "contains"         // group/group/project/branch/directory/file tree
	EdgeOnBranch       EdgeType = "on_branch"        // directory/file → branch
	EdgeHasDiff        EdgeType = "has_diff"         // merge_request → merge_request_diff
	EdgeHasLatestDiff  EdgeType = "has_latest_diff"  // merge_request → merge_request_diff (tip)
	EdgeHasFile        EdgeType = "has_file"         // merge_request_diff → merge_request_diff_file
	EdgeMerged         EdgeType = "merged"           // user → merge_request
	EdgeReviewer       EdgeType = "reviewer"         // user → merge_request
	EdgeInProject      EdgeType = "in_project"       // {mr,diff,difffile,finding,occurrence,identifier,scan,scanner,vulnerability} → project
	EdgeInPipeline     EdgeType = "in_pipeline"      // security_scan → ci (pipeline)
	EdgeHasFinding     EdgeType = "has_finding"      // vulnerability/security_scan → occurrence/finding
	EdgeOccurrenceOf   EdgeType = "occurrence_of"    // vulnerability_occurrence → vulnerability
	EdgeDetectedBy     EdgeType = "detected_by"      // finding/occurrence → vulnerability_scanner
	EdgeHasIdentifier  EdgeType = "has_identifier"   // finding/occurrence → vulnerability_identifier
	EdgeFixes          EdgeType = "fixes"            // merge_request → vulnerability
	EdgeScans          EdgeType = "scans"            // vulnerability_scanner → project
	EdgeResolvedBy     EdgeType = "resolved_by"      // user → vulnerability
	EdgeConfirmedBy    EdgeType = "confirmed_by"     // user → vulnerability
	EdgeDismissedBy    EdgeType = "dismissed_by"     // user → vulnerability
	EdgeMergedAtCommit EdgeType = "merged_at_commit" // merge_request → commit
)

// Direction selects the traversal direction relative to the start entity.
type Direction string

const (
	Outgoing Direction = "outgoing"
	Incoming Direction = "incoming"
)

// Mode selects the query family executed by query_graph (DSL `mode`, plan
// R1–R4). The zero value means traversal, so existing callers are unchanged.
type Mode string

const (
	ModeTraversal   Mode = "traversal"
	ModeAggregation Mode = "aggregation"
	ModePathFinding Mode = "path_finding"
	ModeNeighbors   Mode = "neighbors"
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

// Query is one graph request: mode, start entity, selectors, and bounds.
type Query struct {
	// Mode selects the query family; empty defaults to ModeTraversal and
	// preserves the pre-parity behaviour.
	Mode Mode
	// StartEntity selects the start-node family; empty defaults to definition.
	StartEntity Entity
	Start       Filter
	Filter      Filter
	// EdgeType restricts expansion edges; empty or "*" means any edge type.
	EdgeType  EdgeType
	Direction Direction
	Hops      int
	Limit     int
	// FromKey/ToKey are the canonical endpoint keys (payload node_key,
	// data-model §0) of a path_finding request.
	FromKey string
	ToKey   string
	// NodeKey is the canonical node_key of the neighbours centre.
	NodeKey string
	// RelTypes restricts path_finding/neighbors edges by relation type
	// (edge_type values). path_finding requires a non-empty list (T010);
	// neighbors treats an empty list as all relations.
	RelTypes []string
	// Aggregation knobs (mode=aggregation, plan §7.1): AggOp is
	// count|sum|avg|min|max, AggField the target field, GroupBy the group
	// column, Sort "col" (asc) or "-col" (desc). Plain data: validation and
	// execution live in dsl.go/aggregate.go.
	AggOp    string
	AggField string
	GroupBy  string
	Sort     string
}
