package graph

import (
	"strings"

	"github.com/qdrant/go-client/qdrant"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// Schema-v3 ownership family (T036/T037, US4; data-model §new nodes): the
// group and project nodes of the ownership chain and the group→group /
// group→project half of the contains tree. The builders are pure: no client,
// no I/O. Like every schema-v3 family the deterministic NodeID/EdgeID input
// IS the canonical Ref* key (no opaque Key* helper) and generation stays out
// of the ID (it lives in the payload), so a re-index rewrites the same point.
//
// The group→group and group→project contains edges are the generic
// structure.go builder (it already takes arbitrary endpoint keys): the two
// named wrappers below pin the endpoint families for the ownership callers.
//
// The optional creator (user→project) relation is intentionally NOT emitted:
// the label has no entry in the frozen relation taxonomy (types.go,
// dsl.go knownEdgeType, graph_schema T011, contracts/graph-schema-additions.md),
// so a creator edge must land together with its schema registration; the
// project node already carries creator_key (RefUser) for that future step.

// GroupPoint builds the group node (kind=group, T036): one namespace group of
// the provider listing. node_key is RefGroup(full_path) — namespace-global,
// like RefUser — while the point ID stays repo/branch-scoped like every
// sibling, so the sweep (gc.go) can generation-sweep it per pass. parent_key
// is the parent group's RefGroup derived from the namespace hierarchy (GitLab
// full_path = parent.full_path + "/" + path), empty for a top-level group.
// Pure: no client, no I/O.
func GroupPoint(meta Meta, g provider.Group) *qdrant.PointStruct {
	key := RefGroup(g.FullPath)
	parent := ""
	if i := strings.LastIndex(g.FullPath, "/"); i > 0 {
		parent = RefGroup(g.FullPath[:i])
	}
	return &qdrant.PointStruct{
		Id: qdrant.NewID(NodeID(Namespace(meta.SchemaVer), meta.Repo, meta.Branch,
			KindGroup, key).String()),
		Payload: structureNodePayload(meta, KindGroup, key, map[string]any{
			"group_id":   g.ID,
			"full_path":  g.FullPath,
			"path":       g.Path,
			"name":       g.Name,
			"parent_key": parent,
		}),
	}
}

// ProjectPoint builds the project node (kind=project, T036): one project of a
// group listing. node_key is RefProject(full_path) — the repo identity, the
// same key the review/security families point at with in_project. host is the
// provider instance; creator_key is the creator's RefUser(host, provider user
// id) and stays empty when the provider exposes no creator id. Pure: no
// client, no I/O.
func ProjectPoint(meta Meta, host string, p provider.Project) *qdrant.PointStruct {
	key := RefProject(p.FullPath)
	creator := ""
	if p.CreatorID != "" {
		creator = RefUser(host, p.CreatorID)
	}
	return &qdrant.PointStruct{
		Id: qdrant.NewID(NodeID(Namespace(meta.SchemaVer), meta.Repo, meta.Branch,
			KindProject, key).String()),
		Payload: structureNodePayload(meta, KindProject, key, map[string]any{
			"project_id":     p.ID,
			"full_path":      p.FullPath,
			"path":           p.Path,
			"name":           p.Name,
			"namespace_kind": p.NamespaceKind,
			"creator_key":    creator,
		}),
	}
}

// GroupContainsGroupEdge — parent group → subgroup (T037): one ownership step
// of the group tree. Thin wrapper over the generic structure.go ContainsEdge
// (reused, not re-implemented) that pins the group→group endpoint families.
func GroupContainsGroupEdge(meta Meta, parentKey, childKey string) *qdrant.PointStruct {
	return ContainsEdge(meta, parentKey, "group", childKey, "group")
}

// GroupContainsProjectEdge — group → project (T037): the US4 acceptance path
// (group → project reachable). Same generic ContainsEdge, group→project
// endpoint families pinned.
func GroupContainsProjectEdge(meta Meta, groupKey, projectKey string) *qdrant.PointStruct {
	return ContainsEdge(meta, groupKey, "group", projectKey, "project")
}
