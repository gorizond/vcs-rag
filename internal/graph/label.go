package graph

import (
	"time"

	"github.com/qdrant/go-client/qdrant"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// LabelPoint builds the label node (kind=label, T030/US3). node_key and
// label_key are both the canonical label ref (RefLabel): the normalized label
// carries no provider id, so scope+name IS its identity — label_key repeats it
// as the family's own identity column, like item_id/ms_id do. scope is
// "group"|"project" on GitLab, empty on GitHub. Pure: no client, no I/O.
func LabelPoint(meta Meta, l provider.Label) *qdrant.PointStruct {
	ref := RefLabel(l.Scope, l.Name)
	return &qdrant.PointStruct{
		Id: qdrant.NewID(NodeID(Namespace(meta.SchemaVer), meta.Repo, meta.Branch,
			KindLabel, KeyLabel(l.Scope, l.Name)).String()),
		Payload: valueMap(map[string]any{
			"kind":       string(KindLabel),
			"node_key":   ref,
			"label_key":  ref,
			"name":       l.Name,
			"scope":      l.Scope,
			"repo":       meta.Repo,
			"group_org":  meta.GroupOrg,
			"vcs_type":   meta.VCSType,
			"branch":     meta.Branch,
			"schema_ver": meta.SchemaVer,
			"generation": meta.Generation,
			"indexed_at": time.Now().UTC().Format(time.RFC3339),
		}),
	}
}

// LabelledEdge — work_item → label, keyed by the label's scope+name exactly as
// LabelPoint keys the node.
func LabelledEdge(meta Meta, item provider.WorkItem, scope, name string) *qdrant.PointStruct {
	return workItemEdge(meta, EdgeLabelled, item,
		RefLabel(scope, name), "label", KeyLabel(scope, name))
}
