package graph_test

import (
	"testing"

	"github.com/gorizond/vcs-rag/internal/graph"
	"github.com/gorizond/vcs-rag/internal/provider"
)

// TestRunnerPointDeterministicAndIdentified — T036/US4: the runner writer keys
// the node on host+provider id (a renamed description rewrites the same node)
// and carries the canonical node_key the executed_by edges resolve to, with
// the tag list stored as a keyword array.
func TestRunnerPointDeterministicAndIdentified(t *testing.T) {
	meta := graph.Meta{Repo: "org/repo", GroupOrg: "org", VCSType: "gitlab", Branch: "main", SchemaVer: "1", Generation: 7}
	r := provider.Runner{ID: 5, Description: "docker-runner", State: "online", RunnerType: "project", Tags: []string{"docker", "linux"}, Online: true}
	p := graph.RunnerPoint(meta, "gitlab.com", r)
	if p.GetId().GetUuid() != graph.RunnerPoint(meta, "gitlab.com", r).GetId().GetUuid() {
		t.Fatal("RunnerPoint ID must be deterministic")
	}
	renamed := r
	renamed.Description = "docker-runner-2"
	if graph.RunnerPoint(meta, "gitlab.com", renamed).GetId().GetUuid() != p.GetId().GetUuid() {
		t.Fatal("RunnerPoint ID must key on host+id, not the mutable description")
	}
	if graph.RunnerPoint(meta, "gitlab.com", provider.Runner{ID: 6}).GetId().GetUuid() == p.GetId().GetUuid() {
		t.Fatal("RunnerPoint ID must separate runner ids")
	}
	if graph.RunnerPoint(meta, "github.com", r).GetId().GetUuid() == p.GetId().GetUuid() {
		t.Fatal("RunnerPoint ID must separate provider hosts")
	}

	m := p.GetPayload()
	for field, want := range map[string]string{
		"kind":        "runner",
		"node_key":    graph.RefRunner("gitlab.com", "5"),
		"runner_id":   "5",
		"vcs_host":    "gitlab.com",
		"description": "docker-runner",
		"state":       "online",
		"runner_type": "project",
		"repo":        meta.Repo,
		"branch":      meta.Branch,
		"schema_ver":  meta.SchemaVer,
	} {
		if got := m[field].GetStringValue(); got != want {
			t.Errorf("RunnerPoint payload[%s] = %q, want %q", field, got, want)
		}
	}
	tags := m["tags"].GetListValue().GetValues()
	if len(tags) != 2 || tags[0].GetStringValue() != "docker" || tags[1].GetStringValue() != "linux" {
		t.Errorf("RunnerPoint payload[tags] = %v, want [docker linux]", tags)
	}
	if _, ok := graph.RunnerPoint(meta, "gitlab.com", provider.Runner{ID: 7}).GetPayload()["tags"]; ok {
		t.Error("an empty tag list must be omitted from the payload")
	}
}

// TestRunnerPointNameIdentity — T036/US4: GitHub's job-derived runners have no
// numeric id; the reader stores the runner name in Description and it becomes
// the node identity.
func TestRunnerPointNameIdentity(t *testing.T) {
	meta := graph.Meta{Repo: "org/repo", VCSType: "github", Branch: "main", SchemaVer: "1", Generation: 7}
	a := graph.RunnerPoint(meta, "github.com", provider.Runner{Description: "ubuntu-latest"})
	if b := graph.RunnerPoint(meta, "github.com", provider.Runner{Description: "ubuntu-latest"}); a.GetId().GetUuid() != b.GetId().GetUuid() {
		t.Fatal("name-derived RunnerPoint ID must be deterministic")
	}
	if a.GetId().GetUuid() == graph.RunnerPoint(meta, "github.com", provider.Runner{Description: "ubuntu-22.04"}).GetId().GetUuid() {
		t.Fatal("name-derived RunnerPoint ID must separate runner names")
	}
	m := a.GetPayload()
	if got := m["node_key"].GetStringValue(); got != graph.RefRunner("github.com", "ubuntu-latest") {
		t.Errorf("RunnerPoint payload[node_key] = %q, want %q", got, graph.RefRunner("github.com", "ubuntu-latest"))
	}
	if got := m["runner_id"].GetStringValue(); got != "ubuntu-latest" {
		t.Errorf("RunnerPoint payload[runner_id] = %q, want the runner name", got)
	}
}

// TestExecutedByEdgeShape — T036/US4: executed_by runs ci → runner; the
// endpoints are the canonical node_key refs the neighbors/DSL traversal
// resolves.
func TestExecutedByEdgeShape(t *testing.T) {
	meta := graph.Meta{Repo: "org/repo", VCSType: "gitlab", Branch: "main", SchemaVer: "1", Generation: 7}
	ci, runner := "org/repo|ci:job#42/build", graph.RefRunner("gitlab.com", "5")
	e := graph.ExecutedByEdge(meta, ci, runner)
	if e.GetId().GetUuid() != graph.ExecutedByEdge(meta, ci, runner).GetId().GetUuid() {
		t.Fatal("ExecutedByEdge ID must be deterministic")
	}
	m := e.GetPayload()
	for field, want := range map[string]string{
		"kind": "edge", "edge_type": "executed_by",
		"from_key": ci, "from_type": "ci",
		"to_key": runner, "to_type": "runner",
		"repo": meta.Repo, "branch": meta.Branch,
	} {
		if got := m[field].GetStringValue(); got != want {
			t.Errorf("ExecutedByEdge payload[%s] = %q, want %q", field, got, want)
		}
	}
}
