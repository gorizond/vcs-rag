package worker

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"github.com/qdrant/go-client/qdrant"

	"github.com/gorizond/vcs-rag/internal/config"
	"github.com/gorizond/vcs-rag/internal/graph"
	"github.com/gorizond/vcs-rag/internal/provider"
)

// ownershipStub serves one fixed instance-level group/project fixture: the
// top-level group g, its subgroup g/sub holding one project. The embedded nil
// VCSProvider panics on any other method, proving the step calls only the
// GroupReader seam.
type ownershipStub struct {
	provider.VCSProvider
}

func (s *ownershipStub) ListGroups(context.Context) ([]provider.Group, error) {
	return []provider.Group{
		{ID: "1", FullPath: "g", Path: "g", Name: "g"},
		{ID: "2", FullPath: "g/sub", Path: "sub", Name: "sub"},
	}, nil
}

func (s *ownershipStub) ListGroupProjects(_ context.Context, groupPath string) ([]provider.Project, error) {
	if groupPath != "g/sub" {
		return nil, nil
	}
	return []provider.Project{
		{ID: "10", FullPath: "g/sub/r", Path: "r", Name: "r", NamespaceKind: "group"},
	}, nil
}

// pointIDs is the sorted ID list of a point set — the observable identity of
// the emitted points.
func pointIDs(pts []*qdrant.PointStruct) []string {
	ids := make([]string, 0, len(pts))
	for _, p := range pts {
		ids = append(ids, p.GetId().GetUuid())
	}
	sort.Strings(ids)
	return ids
}

// TestOwnershipPointsScopeIndependent — ownership is instance-level: the same
// group/project listing emitted from two different repo passes must yield the
// same point IDs (with an empty payload scope), or every scanning repo adds
// its own duplicate copy of the whole group/project tree.
func TestOwnershipPointsScopeIndependent(t *testing.T) {
	deps := RepoDeps{VCSType: "gitlab", VCS: &ownershipStub{}, Logger: testLogger()}
	base := graph.Meta{VCSType: "gitlab", SchemaVer: "4", Generation: 42}
	a := base
	a.Repo, a.Branch = "a/x", "main"
	b := base
	b.Repo, b.Branch = "b/y", "develop"

	pa, okA := ownershipPoints(context.Background(), deps, a, "gitlab.example", deps.Logger)
	pb, okB := ownershipPoints(context.Background(), deps, b, "gitlab.example", deps.Logger)
	if !okA || !okB || len(pa) == 0 || len(pb) == 0 {
		t.Fatalf("ownershipPoints: a=(%d,%v) b=(%d,%v), want completed non-empty listings",
			len(pa), okA, len(pb), okB)
	}

	idsA, idsB := pointIDs(pa), pointIDs(pb)
	if !reflect.DeepEqual(idsA, idsB) {
		t.Fatalf("point IDs depend on the scanning repo:\n a: %v\n b: %v", idsA, idsB)
	}
	// The empty payload scope is what makes the points sweepable by the
	// repo-agnostic ownership sweep (gc.go).
	for _, p := range pa {
		m := p.GetPayload()
		if v, ok := m["repo"]; !ok || v.GetStringValue() != "" {
			t.Fatalf("ownership point %s carries a repo scope: %v", p.GetId().GetUuid(), m)
		}
		if v, ok := m["branch"]; !ok || v.GetStringValue() != "" {
			t.Fatalf("ownership point %s carries a branch scope: %v", p.GetId().GetUuid(), m)
		}
	}

	// Negative control: the schema version still namespaces the IDs apart —
	// the step drops the repo/branch scope only.
	c := a
	c.SchemaVer = "5"
	pc, _ := ownershipPoints(context.Background(), deps, c, "gitlab.example", deps.Logger)
	if reflect.DeepEqual(idsA, pointIDs(pc)) {
		t.Fatal("schema version must stay part of the point ID")
	}
}

// planningStub serves the pass-level ownership listing plus the planning
// families of one repository, all against one fixed neutral fixture: the
// instance-level group example-org with project example-org/r, and a milestone,
// a group-scoped label and a work item of that repository. The embedded nil
// VCSProvider panics on any other method, proving the resolution test calls
// only the GroupReader and planning seams.
type planningStub struct {
	provider.VCSProvider
}

func (s *planningStub) ListGroups(context.Context) ([]provider.Group, error) {
	return []provider.Group{
		{ID: "1", FullPath: "example-org", Path: "example-org", Name: "example-org"},
	}, nil
}

func (s *planningStub) ListGroupProjects(context.Context, string) ([]provider.Project, error) {
	return []provider.Project{
		{ID: "10", FullPath: "example-org/r", Path: "r", Name: "r", NamespaceKind: "group"},
	}, nil
}

func (s *planningStub) ListMilestones(context.Context, provider.Repo) ([]provider.Milestone, error) {
	return []provider.Milestone{{ID: 3, Title: "v1"}}, nil
}

func (s *planningStub) ListLabels(context.Context, provider.Repo) ([]provider.Label, error) {
	return []provider.Label{{Name: "area", Scope: "group"}}, nil
}

func (s *planningStub) ListWorkItems(context.Context, provider.Repo) ([]provider.WorkItem, error) {
	return []provider.WorkItem{{ID: 7, ItemType: "issue", Title: "fix"}}, nil
}

// TestInGroupResolvesAgainstPassLevelKeys — the ownership step is pass-level,
// so the per-repo step receives the group keys of the whole pass (exactly the
// map graphPass computes from the pass-level listing and threads in), not its
// own listing. The planning entities of the repository resolve their in_group
// edges against those keys; with the keys withheld (no completed ownership
// listing) the resolve-or-skip rule emits none instead of dangling an
// endpoint.
func TestInGroupResolvesAgainstPassLevelKeys(t *testing.T) {
	deps := RepoDeps{VCSType: "gitlab", VCS: &planningStub{}, Logger: testLogger()}
	pass := graph.Meta{VCSType: "gitlab", SchemaVer: "4", Generation: 42}
	repo := provider.Repo{Namespace: "example-org", Path: "r"}
	meta := pass
	meta.Repo, meta.GroupOrg, meta.Branch = "example-org/r", "example-org", "main"

	ownerPts, ok := ownershipPoints(context.Background(), deps, pass, "gitlab.example", deps.Logger)
	if !ok || len(ownerPts) == 0 {
		t.Fatalf("ownershipPoints: (%d,%v), want a completed non-empty listing", len(ownerPts), ok)
	}
	keys := groupKeys(ownerPts)
	groupKey := graph.RefGroup(meta.GroupOrg)
	if !keys[groupKey] {
		t.Fatalf("pass-level keys miss %q: %v", groupKey, keys)
	}

	pts, _, _ := workItemPoints(context.Background(), deps, repo, meta, "gitlab.example", keys, deps.Logger)
	want := map[string]string{
		"milestone": graph.RefMilestone(meta.Repo, "3"),
		"label":     graph.RefLabel("group", "area"),
		"work_item": graph.RefWorkItem(meta.Repo, "issue", "7"),
	}
	got := map[string]string{}
	for _, p := range pts {
		m := p.GetPayload()
		if m["kind"].GetStringValue() != string(graph.KindEdge) || m["edge_type"].GetStringValue() != string(graph.EdgeInGroup) {
			continue
		}
		if to := m["to_key"].GetStringValue(); to != groupKey {
			t.Fatalf("in_group edge targets %q, want %q", to, groupKey)
		}
		got[m["from_type"].GetStringValue()] = m["from_key"].GetStringValue()
	}
	for fromType, fromKey := range want {
		if got[fromType] != fromKey {
			t.Fatalf("in_group %s→group: got %q, want %q (all: %v)", fromType, got[fromType], fromKey, got)
		}
	}

	// Negative control: without the pass-level keys every in_group edge is
	// withheld.
	pts, _, _ = workItemPoints(context.Background(), deps, repo, meta, "gitlab.example", nil, deps.Logger)
	for _, p := range pts {
		if m := p.GetPayload(); m["edge_type"].GetStringValue() == string(graph.EdgeInGroup) {
			t.Fatalf("in_group edge emitted without pass-level keys: %v", m)
		}
	}
}

// graphStoreFake is the graph pass's store seam with recorded calls: it serves
// the ownership upsert and the count/delete/scroll surface of the instance
// sweep without a Qdrant server.
type graphStoreFake struct {
	upserts int
	counts  int
	deletes int
}

func (f *graphStoreFake) Upsert(context.Context, []*qdrant.PointStruct) error {
	f.upserts++
	return nil
}

func (f *graphStoreFake) CountByFilter(context.Context, *qdrant.Filter) (uint64, error) {
	f.counts++
	return 0, nil
}

func (f *graphStoreFake) DeleteByFilter(context.Context, *qdrant.Filter) error {
	f.deletes++
	return nil
}

func (f *graphStoreFake) ScrollAll(context.Context, *qdrant.Filter, uint32) ([]*qdrant.RetrievedPoint, error) {
	return nil, nil
}

// TestGraphPassSweepsOwnershipWithoutRepos — the instance-wide ownership sweep
// runs BEFORE the per-repo loop, so an empty snapshot (no repository to
// process) still emits the ownership points and runs the sweep exactly once;
// the repo loop never runs. A long (or stuck) per-repo loop therefore cannot
// starve the instance sweep.
func TestGraphPassSweepsOwnershipWithoutRepos(t *testing.T) {
	deps := RepoDeps{VCSType: "gitlab", VCS: &ownershipStub{}, Logger: testLogger()}
	cfg := &config.Config{Worker: config.Worker{GraphSchemaVer: "4"}}
	gs := &graphStoreFake{}
	snap := &snapshot{} // empty snapshot: nothing for the repo loop to process

	sum := graphPass(context.Background(), cfg, deps, snap, gs, 0, &vulnGate{}, &runnerGate{})

	if sum.repos.Load() != 0 {
		t.Fatalf("repo loop ran with an empty snapshot: repos=%d", sum.repos.Load())
	}
	if gs.upserts != 1 {
		t.Fatalf("ownership points upserted %d times, want 1", gs.upserts)
	}
	// One count per swept kind (group, project): the sweep path ran even though
	// the pass processed no repository.
	if gs.counts != 2 {
		t.Fatalf("instance sweep counted %d filters, want 2 (group, project) — the sweep must not be gated behind the repo loop", gs.counts)
	}
}
