package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/qdrant/go-client/qdrant"
)

// maxUpsertBatch — write batch size (headroom against the gRPC limit; refined by facts).
const maxUpsertBatch = 128

// Point — a point to write.
type Point struct {
	ID      string
	Vector  []float32
	Payload map[string]any
}

// sanitizeValue makes every string (recursively) valid UTF-8 (T158): the
// Qdrant client panics on invalid UTF-8 (qdrant.NewValueMap → TryValueMap →
// panic), and repository contents and names may contain arbitrary bytes.
// Invalid sequences become U+FFFD; everything else is preserved.
func sanitizeValue(v any) any {
	switch t := v.(type) {
	case string:
		return strings.ToValidUTF8(t, "\uFFFD")
	case []string:
		out := make([]string, len(t))
		for i, s := range t {
			out[i] = strings.ToValidUTF8(s, "\uFFFD")
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = sanitizeValue(item)
		}
		return out
	case map[string]any:
		return sanitizePayload(t)
	default:
		return v
	}
}

// sanitizePayload sanitizes the payload keys and values.
func sanitizePayload(payload map[string]any) map[string]any {
	out := make(map[string]any, len(payload))
	for k, v := range payload {
		out[strings.ToValidUTF8(k, "\uFFFD")] = sanitizeValue(v)
	}
	return out
}

// Upsert writes points in batches ≤ maxUpsertBatch; the last batch uses Wait=true.
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
				// T158: qdrant.NewValueMap panics on invalid UTF-8, and file
				// contents/names from git may contain arbitrary bytes.
				Payload: qdrant.NewValueMap(sanitizePayload(p.Payload)),
			})
		}
		if _, err := s.client.Upsert(ctx, req); err != nil {
			return fmt.Errorf("qdrant: upsert: %w", err)
		}
	}
	return nil
}

// PayloadValues converts a payload map into Qdrant values (the upsert
// value-building: sanitize + NewValueMap) — the input of SetPayload.
func PayloadValues(payload map[string]any) map[string]*qdrant.Value {
	return qdrant.NewValueMap(sanitizePayload(payload))
}

// SetPayload merges payload fields into existing points without touching their
// vectors (F3/T029): a re-upsert would re-embed the batch and break the LM
// budget, so additive fields of already-embedded points go through here.
func (s *Store) SetPayload(ctx context.Context, ids []string, payload map[string]*qdrant.Value) error {
	if len(ids) == 0 || len(payload) == 0 {
		return nil
	}
	ptIDs := make([]*qdrant.PointId, 0, len(ids))
	for _, id := range ids {
		ptIDs = append(ptIDs, qdrant.NewID(id))
	}
	_, err := s.client.SetPayload(ctx, &qdrant.SetPayloadPoints{
		CollectionName: s.collection,
		Wait:           qdrant.PtrOf(true),
		Payload:        payload,
		PointsSelector: qdrant.NewPointsSelector(ptIDs...),
	})
	if err != nil {
		return fmt.Errorf("qdrant: set payload: %w", err)
	}
	return nil
}
