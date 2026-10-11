package store

import (
	"context"
	"fmt"

	"github.com/qdrant/go-client/qdrant"
)

// RepoRef — one repository as stored in the payload (T133-fix, live-verified):
// the worker writes repo=<short path> (r.Path) and group_org=<full namespace>
// (r.Namespace). Matching "repo" against a full "ns/path" gives 0 points
// (vcs_rag_<contour>: repo="databases" → 82, repo="example-orgexample-group/
// gitops/databases" → 0; vcs_rag: "runner" → 9, "secrets/runner" → 0), so a
// resolved filter carries the pair, never the dashed full name.
type RepoRef struct {
	GroupOrg string // payload group_org: the full namespace
	Repo     string // payload repo: the short repository path
}

// Filter — payload search filter; empty fields do not participate
// (must conditions on payload indexes, no full scan).
type Filter struct {
	VCSType   string
	GroupOrg  string
	Repo      string    // one repository (short form as stored); raw passthrough when unresolved
	RepoRefs  []RepoRef // T133-fix: resolved pairs; 1 — direct pair, 2+ — OR (pairs win over GroupOrg/Repo)
	Branch    string
	Type      string
	State     string // canonical open|merged|closed|locked|draft (F3)
	Author    string
	Milestone string
	// Array fields (F3): one value — Match (Qdrant matches any array element),
	// several — MatchAny (ANY semantics); empty — no condition.
	Labels    []string
	Assignees []string
	Reviewers []string
}

// ScoredPoint — a vector search result.
type ScoredPoint struct {
	ID      string
	Score   float32
	Payload map[string]any
}

// buildFilter translates Filter into a Qdrant payload filter; nil means "no
// conditions at all". A single repository (Repo or one RepoRef) is a Must
// pair; 2+ RepoRefs become an OR of nested (group_org AND repo) filters with
// min_should=1 — T133-fix.
func buildFilter(f Filter) *qdrant.Filter {
	var must []*qdrant.Condition
	add := func(key, value string) {
		if value != "" {
			must = append(must, qdrant.NewMatch(key, value))
		}
	}
	// addAny — arrays (F3): Match for one value (any element), MatchAny for
	// several; an empty list is no condition.
	addAny := func(key string, values []string) {
		switch len(values) {
		case 0:
		case 1:
			must = append(must, qdrant.NewMatch(key, values[0]))
		default:
			must = append(must, qdrant.NewMatchKeywords(key, values...))
		}
	}
	add("vcs_type", f.VCSType)
	switch len(f.RepoRefs) {
	case 0:
		add("group_org", f.GroupOrg)
		add("repo", f.Repo)
	case 1:
		add("group_org", f.RepoRefs[0].GroupOrg)
		add("repo", f.RepoRefs[0].Repo)
	}
	add("branch", f.Branch)
	add("type", f.Type)
	add("state", f.State)
	add("author", f.Author)
	add("milestone", f.Milestone)
	addAny("labels", f.Labels)
	addAny("assignees", f.Assignees)
	addAny("reviewers", f.Reviewers)

	var flt *qdrant.Filter
	if len(must) > 0 {
		flt = &qdrant.Filter{Must: must}
	}
	if len(f.RepoRefs) > 1 {
		should := make([]*qdrant.Condition, 0, len(f.RepoRefs))
		for _, ref := range f.RepoRefs {
			inner := &qdrant.Filter{}
			if ref.GroupOrg != "" {
				inner.Must = append(inner.Must, qdrant.NewMatch("group_org", ref.GroupOrg))
			}
			inner.Must = append(inner.Must, qdrant.NewMatch("repo", ref.Repo))
			should = append(should, qdrant.NewFilterAsCondition(inner))
		}
		if flt == nil {
			flt = &qdrant.Filter{}
		}
		flt.Should = should
		flt.MinShould = &qdrant.MinShould{Conditions: should, MinCount: 1}
	}
	return flt
}

// Search performs a filtered vector search; topK sets the result limit.
func (s *Store) Search(ctx context.Context, vector []float32, f Filter, topK int) ([]ScoredPoint, error) {
	req := &qdrant.QueryPoints{
		CollectionName: s.collection,
		Query:          qdrant.NewQuery(vector...),
		Limit:          qdrant.PtrOf(uint64(topK)),
		WithPayload:    qdrant.NewWithPayloadEnable(true),
	}
	req.Filter = buildFilter(f)

	pts, err := s.client.Query(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("qdrant: search: %w", err)
	}
	out := make([]ScoredPoint, 0, len(pts))
	for _, p := range pts {
		out = append(out, ScoredPoint{
			ID:      p.GetId().GetUuid(),
			Score:   p.GetScore(),
			Payload: qdrant.ValueMapToMap(p.GetPayload()),
		})
	}
	return out, nil
}

// Count returns the exact number of collection points (read-back for e2e).
func (s *Store) Count(ctx context.Context) (uint64, error) {
	n, err := s.client.Count(ctx, &qdrant.CountPoints{
		CollectionName: s.collection,
		Exact:          qdrant.PtrOf(true),
	})
	if err != nil {
		return 0, fmt.Errorf("qdrant: count: %w", err)
	}
	return n, nil
}

// CountByFilter — the number of points under a filter (prune checks).
func (s *Store) CountByFilter(ctx context.Context, f *qdrant.Filter) (uint64, error) {
	n, err := s.client.Count(ctx, &qdrant.CountPoints{
		CollectionName: s.collection,
		Filter:         f,
		Exact:          qdrant.PtrOf(true),
	})
	if err != nil {
		return 0, fmt.Errorf("qdrant: count: %w", err)
	}
	return n, nil
}

// ScrollPayloads returns the payloads of the first limit points (read-back for e2e).
func (s *Store) ScrollPayloads(ctx context.Context, limit uint32) ([]map[string]any, error) {
	pts, err := s.scrollByFilter(ctx, nil, limit)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(pts))
	for _, p := range pts {
		out = append(out, p.Payload)
	}
	return out, nil
}

// ScrollCIFailures returns the existing ci points of a repository (prune).
// ponytail: one page up to limit; tails beyond limit are handled by the next cycle.
func (s *Store) ScrollCIFailures(ctx context.Context, repo string, limit uint32) ([]ScoredPoint, error) {
	return s.scrollByFilter(ctx, &qdrant.Filter{Must: []*qdrant.Condition{
		qdrant.NewMatch("type", "ci_failure"),
		qdrant.NewMatch("repo", repo),
	}}, limit)
}

// scrollByFilter — one scroll page by filter (nil — no filter).
func (s *Store) scrollByFilter(ctx context.Context, f *qdrant.Filter, limit uint32) ([]ScoredPoint, error) {
	pts, err := s.client.Scroll(ctx, &qdrant.ScrollPoints{
		CollectionName: s.collection,
		Filter:         f,
		Limit:          qdrant.PtrOf(limit),
		WithPayload:    qdrant.NewWithPayloadEnable(true),
	})
	if err != nil {
		return nil, fmt.Errorf("qdrant: scroll: %w", err)
	}
	out := make([]ScoredPoint, 0, len(pts))
	for _, p := range pts {
		out = append(out, ScoredPoint{
			ID:      p.GetId().GetUuid(),
			Payload: qdrant.ValueMapToMap(p.GetPayload()),
		})
	}
	return out, nil
}

// DropCollection deletes the collection (e2e cleanup).
func (s *Store) DropCollection(ctx context.Context) error {
	return s.client.DeleteCollection(ctx, s.collection)
}
