package graph

import (
	"github.com/qdrant/go-client/qdrant"
)

// F13 literal-parity relation builders (schema v4; T017/T018/US3;
// contracts/graph-schema-additions.md): the ontology edges beyond
// merged_at_commit. The builders are pure: no client, no I/O. Like the review
// and security families they key the deterministic EdgeID on the canonical
// endpoint keys (generation stays out of the ID and in the payload), so a
// re-index rewrites the same point; the payload is the shared orbitEdgeMap.
//
// The CI-targeted builders (triggered / has_head_pipeline / child_of /
// created_for_mr) have no provider source in this codebase yet — the
// normalized MR and ci records carry no head-pipeline or parent-pipeline link
// — so nothing calls them: they are declared for the literal-parity catalog
// and land on the flattened ci family, whose ci_level payload field
// discriminates Pipeline/Stage/Job/Environment (research R5). Dormant is not a
// GAP (FR-009). approved/updated_by/last_edited_by have no builders at all:
// the provider exposes no such fields (T019) — types.go declares them and the
// DSL accepts them as edge.type.

// orbitRelationEdge builds one orbit-parity relation point: deterministic ID
// over the canonical endpoint keys with the shared edge payload.
func orbitRelationEdge(meta Meta, et EdgeType, fromKey, fromType, toKey, toType string) *qdrant.PointStruct {
	return &qdrant.PointStruct{
		Id: qdrant.NewID(EdgeID(Namespace(meta.SchemaVer), meta.Repo, meta.Branch,
			et, fromKey, toKey).String()),
		Payload: orbitEdgeMap(meta, et, fromKey, fromType, toKey, toType),
	}
}

// InGroupEdge — milestone|label|work_item → group (T017): the 1:1 ownership
// relation of a group-scoped planning entity. groupKey is the group node's
// canonical ref (RefGroup); fromType names the source family (milestone,
// label, work_item).
func InGroupEdge(meta Meta, fromKey, fromType, groupKey string) *qdrant.PointStruct {
	return orbitRelationEdge(meta, EdgeInGroup, fromKey, fromType, groupKey, "group")
}

// MemberOfEdge — user → group|project (T017): the 1:1 membership relation.
// userKey is the user node's canonical ref (RefUser); targetKey/toType name
// the target family's canonical ref and family (group|project).
func MemberOfEdge(meta Meta, userKey, targetKey, targetType string) *qdrant.PointStruct {
	return orbitRelationEdge(meta, EdgeMemberOf, userKey, "user", targetKey, targetType)
}

// TriggeredEdge — merge_request → ci (T018; dormant): the pipeline a request
// triggered.
func TriggeredEdge(meta Meta, mrKey, ciKey string) *qdrant.PointStruct {
	return orbitRelationEdge(meta, EdgeTriggered, mrKey, "merge_request", ciKey, "ci")
}

// HeadPipelineEdge — merge_request → ci (T018; dormant): the request's head
// pipeline.
func HeadPipelineEdge(meta Meta, mrKey, ciKey string) *qdrant.PointStruct {
	return orbitRelationEdge(meta, EdgeHeadPipeline, mrKey, "merge_request", ciKey, "ci")
}

// ChildOfEdge — ci → ci (T018; dormant): a downstream pipeline and its parent.
// ciKey is the child point's canonical ref (RefCi), parentCiKey the parent's.
func ChildOfEdge(meta Meta, ciKey, parentCiKey string) *qdrant.PointStruct {
	return orbitRelationEdge(meta, EdgeChildOf, ciKey, "ci", parentCiKey, "ci")
}

// CreatedForMrEdge — ci → merge_request (T018; dormant): the environment (of
// the flattened ci family) created for a request.
func CreatedForMrEdge(meta Meta, ciKey, mrKey string) *qdrant.PointStruct {
	return orbitRelationEdge(meta, EdgeCreatedForMr, ciKey, "ci", mrKey, "merge_request")
}
