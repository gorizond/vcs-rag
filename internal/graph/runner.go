package graph

import (
	"strconv"
	"time"

	"github.com/qdrant/go-client/qdrant"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// RunnerPoint builds the runner node of one CI runner (kind=runner, T036/US4).
// host is the provider instance (vcsHost); the canonical key is id-based
// (RefRunner), so a renamed runner rewrites the same point instead of
// orphaning it. GitLab runners carry a numeric id; GitHub's job-derived view
// has only a name, which the reader stores in Description and which doubles as
// the id here. Pure: no client, no I/O.
func RunnerPoint(meta Meta, host string, run provider.Runner) *qdrant.PointStruct {
	id := strconv.Itoa(run.ID)
	if run.ID == 0 && run.Description != "" {
		id = run.Description // GitHub: the runner name IS the identity
	}
	payload := valueMap(map[string]any{
		"kind":        string(KindRunner),
		"node_key":    RefRunner(host, id),
		"runner_id":   id,
		"vcs_host":    host,
		"description": run.Description,
		"state":       run.State,
		"runner_type": run.RunnerType,
		"repo":        meta.Repo,
		"group_org":   meta.GroupOrg,
		"vcs_type":    meta.VCSType,
		"branch":      meta.Branch,
		"schema_ver":  meta.SchemaVer,
		"generation":  meta.Generation,
		"indexed_at":  time.Now().UTC().Format(time.RFC3339),
	})
	// tags is a keyword list; valueMap carries scalars only.
	if len(run.Tags) > 0 {
		vals := make([]*qdrant.Value, 0, len(run.Tags))
		for _, tag := range run.Tags {
			vals = append(vals, qdrant.NewValueString(tag))
		}
		payload["tags"] = qdrant.NewValueList(&qdrant.ListValue{Values: vals})
	}
	return &qdrant.PointStruct{
		Id: qdrant.NewID(NodeID(Namespace(meta.SchemaVer), meta.Repo, meta.Branch,
			KindRunner, KeyRunner(host, id)).String()),
		Payload: payload,
	}
}

// ExecutedByEdge builds the executed_by edge (ci → runner, T036/US4): ciKey and
// runnerKey are the canonical node_key refs of the ci point and the runner
// node. Pure: no client, no I/O.
func ExecutedByEdge(meta Meta, ciKey, runnerKey string) *qdrant.PointStruct {
	return &qdrant.PointStruct{
		Id: qdrant.NewID(EdgeID(Namespace(meta.SchemaVer), meta.Repo, meta.Branch,
			EdgeExecutedBy, ciKey, runnerKey).String()),
		Payload: orbitEdgeMap(meta, EdgeExecutedBy, ciKey, "ci", runnerKey, "runner"),
	}
}
