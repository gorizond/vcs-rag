package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/qdrant/go-client/qdrant"

	"github.com/gorizond/vcs-rag/internal/graph"
	"github.com/gorizond/vcs-rag/internal/provider"
)

// fr024Stub is the per-domain provider seam of the FR-024 check: every domain
// reader returns the configured error (nil drives the healthy listings). The
// embedded nil VCSProvider panics on any other method, proving the family
// helpers call none of them.
type fr024Stub struct {
	provider.VCSProvider
	err error
}

func (s *fr024Stub) Type() string { return "gitlab" }

func (s *fr024Stub) ListSecurityFindings(context.Context, provider.Repo) ([]provider.VulnerabilityFinding, error) {
	if s.err != nil {
		return nil, s.err
	}
	return []provider.VulnerabilityFinding{{ID: "1", UUID: "u1", ScannerID: "s1", ScanID: "sc1"}}, nil
}

func (s *fr024Stub) ListMergeRequestNodes(context.Context, provider.Repo) ([]provider.ChangeRequest, error) {
	if s.err != nil {
		return nil, s.err
	}
	return []provider.ChangeRequest{{ID: 1}}, nil
}

func (s *fr024Stub) ListMergeRequestDiffRefs(context.Context, provider.Repo, int) ([]provider.MergeRequestDiffRef, error) {
	if s.err != nil {
		return nil, s.err
	}
	return nil, nil
}

func (s *fr024Stub) ListTree(context.Context, provider.Repo) ([]provider.TreeEntry, error) {
	return nil, s.err
}

func (s *fr024Stub) ListBranches(context.Context, provider.Repo) ([]provider.Branch, error) {
	if s.err != nil {
		return nil, s.err
	}
	return []provider.Branch{{Name: "main", Default: true}}, nil
}

func (s *fr024Stub) ListGroups(context.Context) ([]provider.Group, error) {
	if s.err != nil {
		return nil, s.err
	}
	return []provider.Group{{ID: "1", FullPath: "g", Path: "g", Name: "g"}}, nil
}

func (s *fr024Stub) ListGroupProjects(context.Context, string) ([]provider.Project, error) {
	if s.err != nil {
		return nil, s.err
	}
	return nil, nil
}

// TestFR024DomainFailureNeverAborts — FR-024: a per-domain provider failure,
// plain or a typed unavailable, leaves the family ingest empty and unswept
// (nil,false) and returns normally — the pass continues past it and nothing is
// rolled back. The healthy run after the failures proves the helpers did not
// latch off: each family still ingests its listing.
func TestFR024DomainFailureNeverAborts(t *testing.T) {
	ctx := context.Background()
	meta := graph.Meta{Repo: "g/r", VCSType: "gitlab", Branch: "main", SchemaVer: "3", Generation: 7}
	repo := provider.Repo{Namespace: "g", Path: "r"}
	host := "gitlab.example"

	families := func(deps RepoDeps) map[string]func() ([]*qdrant.PointStruct, bool) {
		return map[string]func() ([]*qdrant.PointStruct, bool){
			"security": func() ([]*qdrant.PointStruct, bool) {
				return securityPoints(ctx, deps, repo, meta, host, nil, deps.Logger)
			},
			"review": func() ([]*qdrant.PointStruct, bool) {
				return reviewPoints(ctx, deps, repo, meta, nil, deps.Logger)
			},
			"structure": func() ([]*qdrant.PointStruct, bool) {
				return structurePoints(ctx, deps, repo, meta, nil, deps.Logger)
			},
			"ownership": func() ([]*qdrant.PointStruct, bool) {
				return ownershipPoints(ctx, deps, meta, host, deps.Logger)
			},
		}
	}

	for _, tc := range []struct {
		name string
		err  error
	}{
		{"plain provider error", errors.New("provider boom")},
		{"typed domain unavailable", provider.ErrDomainUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps := RepoDeps{VCSType: "gitlab", VCS: &fr024Stub{err: tc.err}, Logger: testLogger()}
			for name, call := range families(deps) {
				pts, ok := call()
				if ok || pts != nil {
					t.Errorf("%s: (%d points, ok=%v), want (nil,false) on %v", name, len(pts), ok, tc.err)
				}
			}
		})
	}

	t.Run("healthy provider after failure", func(t *testing.T) {
		deps := RepoDeps{VCSType: "gitlab", VCS: &fr024Stub{}, Logger: testLogger()}
		for name, call := range families(deps) {
			pts, ok := call()
			if !ok || len(pts) == 0 {
				t.Errorf("%s: (%d points, ok=%v), want a completed non-empty listing", name, len(pts), ok)
			}
		}
	})
}
