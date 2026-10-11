package graph_test

import (
	"testing"

	"github.com/gorizond/vcs-rag/internal/graph"
	"github.com/gorizond/vcs-rag/internal/provider"
)

// TestMrFileCiNodeKeys pins the US2 payload node_keys (T013/T014): the mr_file
// point must expose RefMergeRequestDiffFile(repo, iid, path) and the ci point
// RefCi(repo, level, pipeline, job) — the endpoint keys the worker's
// mrFileKeyIndex/ciPipelineKeys resolve the has_file and in_pipeline edges
// against. A missing key silently skips both edges.
func TestMrFileCiNodeKeys(t *testing.T) {
	meta := graph.Meta{Repo: "org/repo", Branch: "main", SchemaVer: "4"}

	p := graph.MrFilePoint(meta, 42, provider.ChangeRequestFile{OldPath: "old.go", NewPath: "new.go", Status: "renamed"}, "", "opened")
	if got := p.GetPayload()["node_key"].GetStringValue(); got != "org/repo|mr:42|file:new.go" {
		t.Fatalf("mr_file node_key = %q, want the canonical RefMergeRequestDiffFile", got)
	}
	// a deleted entry without NewPath keys on its OldPath, matching file_path.
	p = graph.MrFilePoint(meta, 42, provider.ChangeRequestFile{OldPath: "gone.go", Status: "deleted"}, "", "opened")
	if got := p.GetPayload()["node_key"].GetStringValue(); got != "org/repo|mr:42|file:gone.go" {
		t.Fatalf("mr_file node_key (old path fallback) = %q", got)
	}

	job := graph.CiPoint(meta, graph.CiLevelJob, "100", "build", "test", "success", 0, "main", "", "")
	if got := job.GetPayload()["node_key"].GetStringValue(); got != "org/repo|ci:job:100:build" {
		t.Fatalf("ci (job) node_key = %q, want the canonical RefCi", got)
	}
	dep := graph.CiPoint(meta, graph.CiLevelDeployment, "7", "", "", "success", 0, "main", "", "")
	if got := dep.GetPayload()["node_key"].GetStringValue(); got != "org/repo|ci:deployment:7" {
		t.Fatalf("ci (no job part) node_key = %q, want the job part omitted", got)
	}
}
