package graph

import (
	"time"

	"github.com/qdrant/go-client/qdrant"
)

// CI levels of a ci point (payload ci_level, data-model §5).
const (
	CiLevelStage       = "stage"
	CiLevelJob         = "job"
	CiLevelDeployment  = "deployment"
	CiLevelEnvironment = "environment"
)

// CiPoint builds one ci point (kind=ci, F4/US4): a pipeline job, a deployment
// or an environment. jobName doubles as the identifying key part at every
// level — the job name for "job", the environment name for "environment";
// deployment rows key on their own pipelineID (the deployment id). status and
// stage are the values the provider normalized. The deterministic ID keeps the
// re-upsert each pass idempotent and the point alive for the sweep of its
// generation; node_key exposes the canonical RefCi form the in_pipeline edge
// resolves against (F13). Pure: no client, no I/O.
func CiPoint(meta Meta, level, pipelineID, jobName, stage, status string, durationMS int64, ref, env, commitSHA string) *qdrant.PointStruct {
	return &qdrant.PointStruct{
		Id: qdrant.NewID(NodeID(Namespace(meta.SchemaVer), meta.Repo, meta.Branch,
			KindCi, KeyCi(level, pipelineID, jobName)).String()),
		Payload: valueMap(map[string]any{
			"kind":        string(KindCi),
			"ci_level":    level,
			"pipeline_id": pipelineID,
			"job_name":    jobName,
			"node_key":    RefCi(meta.Repo, level, pipelineID, jobName),
			"stage":       stage,
			"status":      status,
			"duration_ms": int(durationMS),
			"ref":         ref,
			"env":         env,
			"commit_sha":  commitSHA,
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
