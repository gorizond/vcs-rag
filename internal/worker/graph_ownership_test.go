package worker

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"github.com/qdrant/go-client/qdrant"

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
