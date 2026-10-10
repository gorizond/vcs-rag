package store

import (
	"context"
	"fmt"

	"github.com/qdrant/go-client/qdrant"
)

// Count возвращает точное число точек коллекции (read-back для e2e).
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

// CountByFilter — число точек под фильтром (проверки prune).
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

// ScrollPayloads возвращает payload'ы первых limit точек (read-back для e2e).
func (s *Store) ScrollPayloads(ctx context.Context, limit uint32) ([]map[string]any, error) {
	pts, err := s.client.Scroll(ctx, &qdrant.ScrollPoints{
		CollectionName: s.collection,
		Limit:          qdrant.PtrOf(limit),
		WithPayload:    qdrant.NewWithPayloadEnable(true),
	})
	if err != nil {
		return nil, fmt.Errorf("qdrant: scroll: %w", err)
	}
	out := make([]map[string]any, 0, len(pts))
	for _, p := range pts {
		out = append(out, qdrant.ValueMapToMap(p.GetPayload()))
	}
	return out, nil
}

// DropCollection удаляет коллекцию (e2e-очистка).
func (s *Store) DropCollection(ctx context.Context) error {
	return s.client.DeleteCollection(ctx, s.collection)
}
