package store

import (
	"context"
	"fmt"

	"github.com/qdrant/go-client/qdrant"
)

// Filter — payload search filter; empty fields do not participate
// (must conditions on payload indexes, no full scan).
type Filter struct {
	VCSType  string
	GroupOrg string
	Repo     string
	Branch   string
	Type     string
	State    string
}

// ScoredPoint — a vector search result.
type ScoredPoint struct {
	ID      string
	Score   float32
	Payload map[string]any
}

// Search performs a filtered vector search; topK sets the result limit.
func (s *Store) Search(ctx context.Context, vector []float32, f Filter, topK int) ([]ScoredPoint, error) {
	req := &qdrant.QueryPoints{
		CollectionName: s.collection,
		Query:          qdrant.NewQuery(vector...),
		Limit:          qdrant.PtrOf(uint64(topK)),
		WithPayload:    qdrant.NewWithPayloadEnable(true),
	}
	var must []*qdrant.Condition
	add := func(key, value string) {
		if value != "" {
			must = append(must, qdrant.NewMatch(key, value))
		}
	}
	add("vcs_type", f.VCSType)
	add("group_org", f.GroupOrg)
	add("repo", f.Repo)
	add("branch", f.Branch)
	add("type", f.Type)
	add("state", f.State)
	if len(must) > 0 {
		req.Filter = &qdrant.Filter{Must: must}
	}

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
