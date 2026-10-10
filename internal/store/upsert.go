package store

import (
	"context"
	"fmt"

	"github.com/qdrant/go-client/qdrant"
)

// maxUpsertBatch — размер батча записи (запас к лимиту gRPC; уточняется по фактам).
const maxUpsertBatch = 128

// Point — точка для записи.
type Point struct {
	ID      string
	Vector  []float32
	Payload map[string]any
}

// Upsert пишет точки батчами ≤ maxUpsertBatch; последний батч — с Wait=true.
func (s *Store) Upsert(ctx context.Context, points []Point) error {
	for start := 0; start < len(points); start += maxUpsertBatch {
		end := min(start+maxUpsertBatch, len(points))
		batch := points[start:end]

		req := &qdrant.UpsertPoints{
			CollectionName: s.collection,
			Wait:           qdrant.PtrOf(end == len(points)),
			Points:         make([]*qdrant.PointStruct, 0, len(batch)),
		}
		for _, p := range batch {
			req.Points = append(req.Points, &qdrant.PointStruct{
				Id:      qdrant.NewID(p.ID),
				Vectors: qdrant.NewVectors(p.Vector...),
				Payload: qdrant.NewValueMap(p.Payload),
			})
		}
		if _, err := s.client.Upsert(ctx, req); err != nil {
			return fmt.Errorf("qdrant: upsert: %w", err)
		}
	}
	return nil
}
