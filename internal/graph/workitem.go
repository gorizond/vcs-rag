package graph

import (
	"strconv"
	"time"

	"github.com/qdrant/go-client/qdrant"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// WorkItemPoint builds the work_item node of one issue/task (kind=work_item,
// T030/US3). node_key is the canonical ref (RefWorkItem) that the
// authored/assigned_to/labelled/in_milestone edges and the graph DSL resolve
// to; item_id/item_type repeat its identifying parts as indexed columns of
// their own. title is stored but stays unindexed (store.graphIndexFields never
// indexes titles). updated_at is written only when the provider gave one — a
// zero time would index as year 1. Pure: no client, no I/O.
func WorkItemPoint(meta Meta, item provider.WorkItem) *qdrant.PointStruct {
	iid := strconv.Itoa(item.ID)
	payload := map[string]any{
		"kind":       string(KindWorkItem),
		"node_key":   RefWorkItem(meta.Repo, item.ItemType, iid),
		"item_id":    iid,
		"item_type":  item.ItemType,
		"state":      item.State,
		"title":      item.Title,
		"repo":       meta.Repo,
		"group_org":  meta.GroupOrg,
		"vcs_type":   meta.VCSType,
		"branch":     meta.Branch,
		"schema_ver": meta.SchemaVer,
		"generation": meta.Generation,
		"indexed_at": time.Now().UTC().Format(time.RFC3339),
	}
	if !item.UpdatedAt.IsZero() {
		payload["updated_at"] = item.UpdatedAt.UTC().Format(time.RFC3339)
	}
	return &qdrant.PointStruct{
		Id: qdrant.NewID(NodeID(Namespace(meta.SchemaVer), meta.Repo, meta.Branch,
			KindWorkItem, KeyWorkItem(item.ItemType, iid)).String()),
		Payload: valueMap(payload),
	}
}

// workItemEdge builds one relation edge of a work item (kind=edge, T030/US3):
// from_key is the item's canonical ref, to_key/to_type the target endpoint's;
// from_type is always "work_item" for this family. idTo is the target's opaque
// identity key, so re-indexing the same relation keeps its deterministic edge
// ID. Pure: no client, no I/O.
func workItemEdge(meta Meta, et EdgeType, item provider.WorkItem, toKey, toType, idTo string) *qdrant.PointStruct {
	iid := strconv.Itoa(item.ID)
	return &qdrant.PointStruct{
		Id: qdrant.NewID(EdgeID(Namespace(meta.SchemaVer), meta.Repo, meta.Branch, et,
			KeyWorkItem(item.ItemType, iid), idTo).String()),
		Payload: valueMap(map[string]any{
			"kind":       string(KindEdge),
			"edge_type":  string(et),
			"from_key":   RefWorkItem(meta.Repo, item.ItemType, iid),
			"from_type":  "work_item",
			"to_key":     toKey,
			"to_type":    toType,
			"repo":       meta.Repo,
			"branch":     meta.Branch,
			"generation": meta.Generation,
			"indexed_at": time.Now().UTC().Format(time.RFC3339),
		}),
	}
}

// AuthoredEdge — work_item → user (the item author). userKey is the user
// node's canonical ref, RefUser(host, uid), so to_key joins that node's
// node_key by construction.
func AuthoredEdge(meta Meta, item provider.WorkItem, userKey string) *qdrant.PointStruct {
	return workItemEdge(meta, EdgeAuthored, item, userKey, "user", userKey)
}

// AssignedToEdge — work_item → user (one assignee); userKey as in AuthoredEdge.
func AssignedToEdge(meta Meta, item provider.WorkItem, userKey string) *qdrant.PointStruct {
	return workItemEdge(meta, EdgeAssignedTo, item, userKey, "user", userKey)
}
