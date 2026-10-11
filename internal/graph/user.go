package graph

import (
	"time"

	"github.com/qdrant/go-client/qdrant"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// UserPoint builds the user node of one VCS account (kind=user, R6/US2). host is
// the provider instance ("gitlab.com", "github.com", or a self-hosted host); the
// canonical key is id-based (RefUser), so a login rename rewrites the same point
// instead of orphaning it. Pure: no client, no I/O.
func UserPoint(meta Meta, host string, u provider.User) *qdrant.PointStruct {
	return &qdrant.PointStruct{
		Id: qdrant.NewID(NodeID(Namespace(meta.SchemaVer), meta.Repo, meta.Branch,
			KindUser, KeyUser(host, u.ID)).String()),
		Payload: valueMap(map[string]any{
			"kind":        string(KindUser),
			"node_key":    RefUser(host, u.ID),
			"vcs_user_id": u.ID,
			"vcs_host":    host,
			"login":       u.Login,
			"name":        u.Name,
			"state":       u.State,
			"repo":        meta.Repo,
			"group_org":   meta.GroupOrg,
			"vcs_type":    meta.VCSType,
			"branch":      meta.Branch,
			"schema_ver":  meta.SchemaVer,
			"generation":  meta.Generation,
			"indexed_at":  time.Now().UTC().Format(time.RFC3339),
		}),
	}
}

// AuthoredPoint builds the authored edge (work_item → user, R7/US2): workItemKey
// and userKey are the canonical node_key refs of the work item and the user
// (RefWorkItem/RefUser). Pure: no client, no I/O.
func AuthoredPoint(meta Meta, workItemKey, userKey string) *qdrant.PointStruct {
	return &qdrant.PointStruct{
		Id: qdrant.NewID(EdgeID(Namespace(meta.SchemaVer), meta.Repo, meta.Branch,
			EdgeAuthored, workItemKey, userKey).String()),
		Payload: orbitEdgeMap(meta, EdgeAuthored, workItemKey, "work_item", userKey, "user"),
	}
}

// orbitEdgeMap is the shared payload of the orbit-parity relation edges
// (authored/commented): the generic endpoint fields + scope (data-model §0/§6).
func orbitEdgeMap(meta Meta, et EdgeType, fromKey, fromType, toKey, toType string) map[string]*qdrant.Value {
	return valueMap(map[string]any{
		"kind":       string(KindEdge),
		"edge_type":  string(et),
		"from_key":   fromKey,
		"from_type":  fromType,
		"to_key":     toKey,
		"to_type":    toType,
		"repo":       meta.Repo,
		"branch":     meta.Branch,
		"schema_ver": meta.SchemaVer,
		"generation": meta.Generation,
		"indexed_at": time.Now().UTC().Format(time.RFC3339),
	})
}
