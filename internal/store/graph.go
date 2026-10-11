package store

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/qdrant/go-client/qdrant"
)

// GraphStore is a payload-only store bound to the graph collection. It shares
// the gRPC client of the vector Store — no second connection/config path —
// and stores no vectors anywhere (invariant R3: the graph never touches the
// embeddings package).
type GraphStore struct {
	client     *qdrant.Client
	collection string
}

// NewGraphStore binds the existing Store connection to the graph collection
// (config.Qdrant.GraphCollection). The collection name is part of the
// instance identity; config.Load guarantees it differs from the vector one.
func NewGraphStore(s *Store, collection string) *GraphStore {
	return &GraphStore{client: s.client, collection: collection}
}

// EnsureGraphCollection creates the graph collection as a vector-less one
// (`vectors: {}` — an empty VectorsConfig map) and verifies the outcome on the
// server before the first write. There is no dummy-vector fallback: a server
// that rejects (or silently rewrites) a vector-less collection is a hard
// configuration/upgrade error naming QDRANT_GRAPH_COLLECTION.
func (g *GraphStore) EnsureGraphCollection(ctx context.Context) error {
	exists, err := g.client.CollectionExists(ctx, g.collection)
	if err != nil {
		return fmt.Errorf("qdrant: graph collection check: %w", err)
	}
	if !exists {
		// VectorsConfig must be present-but-empty: omitting it lets the server
		// apply a default, while an empty map is the gRPC form of `vectors: {}`.
		err := g.client.CreateCollection(ctx, &qdrant.CreateCollection{
			CollectionName: g.collection,
			VectorsConfig:  qdrant.NewVectorsConfigMap(map[string]*qdrant.VectorParams{}),
		})
		if err != nil {
			return fmt.Errorf(
				"qdrant: QDRANT_GRAPH_COLLECTION=%q: vector-less collection creation rejected (vectors: {}): %w",
				g.collection, err)
		}
	}

	// Runtime gate: read back that the collection really has no vectors.
	info, err := g.client.GetCollectionInfo(ctx, g.collection)
	if err != nil {
		return fmt.Errorf("qdrant: graph collection info: %w", err)
	}
	vc := info.GetConfig().GetParams().GetVectorsConfig()
	if vc.GetParams() != nil || len(vc.GetParamsMap().GetMap()) != 0 {
		return fmt.Errorf(
			"qdrant: QDRANT_GRAPH_COLLECTION=%q: collection has vectors configured, expected a vector-less one (vectors: {}) — no dummy-vector fallback (R3)",
			g.collection)
	}
	return nil
}

// graphIndexFields are the payload indexes of the graph collection; kind,
// edge_type, ... schema_ver, symbol, symbol_kind, ... mr_id, ... are keyword,
// only symbol_text is full-text. commit_sha, patch, signature, generation,
// and indexed_at are deliberately NOT indexed.
var graphIndexFields = []string{
	"kind",
	"node_type",
	"edge_type",
	"ci_level",
	"repo",
	"group_org",
	"vcs_type",
	"branch",
	"language",
	"schema_ver",
	"symbol",
	"symbol_text",
	"symbol_kind",
	"from_file",
	"from_package",
	"to_package",
	"to_symbol",
	"file_path",
	"package",
	"mr_id",
	"state",
	"author",
	"status",
	"severity",
}

// graphOnDiskIndexFields are indexed on disk (long values, many distinct
// values per field); the rest of the payload indexes live in memory.
var graphOnDiskIndexFields = map[string]bool{
	"file_path":  true,
	"package":    true,
	"to_package": true,
	"from_file":  true,
	"repo":       true,
}

// EnsureGraphIndexes creates the payload indexes (idempotent: repeating a
// call for an existing index is harmless, as with the vector collection).
func (g *GraphStore) EnsureGraphIndexes(ctx context.Context) error {
	for _, field := range graphIndexFields {
		req := &qdrant.CreateFieldIndexCollection{
			CollectionName: g.collection,
			FieldName:      field,
			FieldType:      qdrant.FieldType_FieldTypeKeyword.Enum(),
			Wait:           qdrant.PtrOf(true),
		}
		if field == "symbol_text" {
			req.FieldType = qdrant.FieldType_FieldTypeText.Enum()
		}
		if graphOnDiskIndexFields[field] {
			req.FieldIndexParams = &qdrant.PayloadIndexParams{
				IndexParams: &qdrant.PayloadIndexParams_KeywordIndexParams{
					KeywordIndexParams: &qdrant.KeywordIndexParams{OnDisk: qdrant.PtrOf(true)},
				},
			}
		}
		if _, err := g.client.CreateFieldIndex(ctx, req); err != nil {
			return fmt.Errorf("qdrant: graph index %q: %w", field, err)
		}
	}
	return nil
}

// CountByFilter returns the exact number of graph points under a filter.
func (g *GraphStore) CountByFilter(ctx context.Context, f *qdrant.Filter) (uint64, error) {
	n, err := g.client.Count(ctx, &qdrant.CountPoints{
		CollectionName: g.collection,
		Filter:         f,
		Exact:          qdrant.PtrOf(true),
	})
	if err != nil {
		return 0, fmt.Errorf("qdrant: graph count: %w", err)
	}
	return n, nil
}

// DeleteByFilter deletes graph points by filter (Wait=true).
func (g *GraphStore) DeleteByFilter(ctx context.Context, f *qdrant.Filter) error {
	if _, err := g.client.Delete(ctx, &qdrant.DeletePoints{
		CollectionName: g.collection,
		Points:         qdrant.NewPointsSelectorFilter(f),
		Wait:           qdrant.PtrOf(true),
	}); err != nil {
		return fmt.Errorf("qdrant: graph delete: %w", err)
	}
	return nil
}

// Upsert writes payload-only points in batches of at most maxUpsertBatch; the
// last batch uses Wait=true. It mirrors Store.Upsert — same batching, same
// T158 UTF-8 guard — but never sends a real vector: the graph collection is
// vector-less (R2), so a point carrying vectors is a caller bug reported here
// instead of a confusing server error.
//
// Qdrant 1.19 requires an explicit EMPTY vector set on every point of a
// vector-less collection: omitting the field fails with "missing field
// `vector`" / "Expected some vectors". emptyGraphVectors() supplies it.
func (g *GraphStore) Upsert(ctx context.Context, points []*qdrant.PointStruct) error {
	for _, p := range points {
		if !graphVectorsEmpty(p.GetVectors()) {
			return fmt.Errorf("qdrant: graph upsert: point %s carries vectors; the graph collection is vector-less (R2)", pointKey(p.GetId()))
		}
	}
	for start := 0; start < len(points); start += maxUpsertBatch {
		end := min(start+maxUpsertBatch, len(points))
		req := &qdrant.UpsertPoints{
			CollectionName: g.collection,
			Wait:           qdrant.PtrOf(end == len(points)),
			Points:         make([]*qdrant.PointStruct, 0, end-start),
		}
		for _, p := range points[start:end] {
			req.Points = append(req.Points, &qdrant.PointStruct{
				Id:      p.GetId(),
				Vectors: emptyGraphVectors(),
				Payload: sanitizeGraphPayload(p.GetPayload()),
			})
		}
		if _, err := g.client.Upsert(ctx, req); err != nil {
			return fmt.Errorf("qdrant: graph upsert: %w", err)
		}
	}
	return nil
}

// emptyGraphVectors is the explicit empty vector set Qdrant 1.19 requires on
// every point of a vector-less collection (omitting it is a server error:
// "missing field `vector`" / "Expected some vectors").
func emptyGraphVectors() *qdrant.Vectors {
	return qdrant.NewVectorsMap(map[string]*qdrant.Vector{})
}

// graphVectorsEmpty reports whether v carries no usable vector (nil counts as
// empty). A point with a real single or named vector is a caller bug (R2).
func graphVectorsEmpty(v *qdrant.Vectors) bool {
	if v == nil {
		return true
	}
	if v.GetVector() != nil {
		return false
	}
	return len(v.GetVectors().GetVectors()) == 0
}

// sanitizeGraphPayload rewrites every string value of a payload to valid
// UTF-8 (T158): Qdrant expects UTF-8 payload strings and repo paths/names come
// from git, not from Go source. Graph payloads are flat scalars
// (string/bool/int); nested struct/list values are not produced today and are
// passed through unchanged.
func sanitizeGraphPayload(in map[string]*qdrant.Value) map[string]*qdrant.Value {
	out := make(map[string]*qdrant.Value, len(in))
	for k, v := range in {
		if sv, ok := v.GetKind().(*qdrant.Value_StringValue); ok {
			out[k] = qdrant.NewValueString(strings.ToValidUTF8(sv.StringValue, "\uFFFD"))
			continue
		}
		out[k] = v
	}
	return out
}

// maxMatchAny is the per-request ceiling on the number of values in a single
// MatchAny condition; longer lists are chunked into several requests.
const maxMatchAny = 1000

// ScrollAll reads every point matching filter; limit is the page size per
// request. It is the only pagination primitive of the graph store: pages are
// requested following NextPageOffset until the server reports none, so the
// result is complete beyond a single page. The result is ordered by point ID;
// a scan split into several requests is merged back into ID order.
//
// MatchAny handling: an empty MatchAny in a must chain can never match, so
// the call returns no points immediately without a request; lists longer
// than maxMatchAny are split into one request per chunk (a cartesian product
// when several such lists occur). must_not is refused for oversized lists —
// the exclusion cannot be expressed as a union of chunk scans.
func (g *GraphStore) ScrollAll(ctx context.Context, filter *qdrant.Filter, limit uint32) ([]*qdrant.RetrievedPoint, error) {
	if hasEmptyMatchAny(filter) {
		return nil, nil
	}
	variants, err := expandMatchAny(filter)
	if err != nil {
		return nil, err
	}
	var out []*qdrant.RetrievedPoint
	for _, f := range variants {
		pts, err := g.scrollPages(ctx, f, limit)
		if err != nil {
			return nil, err
		}
		out = append(out, pts...)
	}
	if len(variants) == 1 {
		return out, nil
	}
	return sortAndDedup(out), nil
}

// scrollPages pages one request filter with limit-sized pages, following
// NextPageOffset until the server reports nil (the end marker).
func (g *GraphStore) scrollPages(ctx context.Context, f *qdrant.Filter, limit uint32) ([]*qdrant.RetrievedPoint, error) {
	var out []*qdrant.RetrievedPoint
	var offset *qdrant.PointId
	for {
		pts, next, err := g.client.ScrollAndOffset(ctx, &qdrant.ScrollPoints{
			CollectionName: g.collection,
			Filter:         f,
			Offset:         offset,
			Limit:          qdrant.PtrOf(limit),
			WithPayload:    qdrant.NewWithPayloadEnable(true),
		})
		if err != nil {
			return nil, fmt.Errorf("qdrant: graph scroll: %w", err)
		}
		out = append(out, pts...)
		if next == nil {
			return out, nil
		}
		if samePointID(next, offset) {
			// The server repeated the page offset: paginating further would
			// loop forever, fail instead of hanging the pass.
			return nil, fmt.Errorf("qdrant: graph scroll: offset did not advance (%s)", pointKey(next))
		}
		offset = next
	}
}

func samePointID(a, b *qdrant.PointId) bool {
	return a != nil && b != nil && a.GetUuid() == b.GetUuid() && a.GetNum() == b.GetNum()
}

// sortAndDedup restores ascending point-ID order after a multi-request scan
// and drops points matched by more than one request variant.
func sortAndDedup(pts []*qdrant.RetrievedPoint) []*qdrant.RetrievedPoint {
	sort.SliceStable(pts, func(i, j int) bool {
		return pointKey(pts[i].GetId()) < pointKey(pts[j].GetId())
	})
	out := pts[:0]
	for i, p := range pts {
		if i > 0 && pointKey(p.GetId()) == pointKey(pts[i-1].GetId()) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// pointKey renders a point ID for ordering: canonical UUIDs compare in byte
// order as hex strings, numeric IDs are zero-padded to compare numerically.
func pointKey(id *qdrant.PointId) string {
	switch {
	case id == nil:
		return ""
	case id.GetUuid() != "":
		return "u:" + id.GetUuid()
	default:
		return fmt.Sprintf("n:%020d", id.GetNum())
	}
}

// expandMatchAny rewrites filter into equivalent request filters, none
// containing a MatchAny longer than maxMatchAny. Chunks of one list are
// disjoint, so variants do not overlap; scanning all of them and merging
// yields exactly the original result set.
func expandMatchAny(filter *qdrant.Filter) ([]*qdrant.Filter, error) {
	out := []*qdrant.Filter{filter}
	var final []*qdrant.Filter
	for len(out) > 0 {
		cur := out[0]
		out = out[1:]
		parts, changed, err := splitFilter(cur)
		if err != nil {
			return nil, err
		}
		if changed {
			out = append(out, parts...)
			continue
		}
		final = append(final, cur)
	}
	return final, nil
}

// splitFilter replaces every oversized MatchAny in must and should with one
// chunked condition per request and returns the cartesian product of the
// per-list alternatives; changed=false means the filter already fits into a
// single request. Oversized lists under must_not are rejected.
func splitFilter(f *qdrant.Filter) ([]*qdrant.Filter, bool, error) {
	if f == nil {
		return []*qdrant.Filter{nil}, false, nil
	}
	if err := checkOversizedInMustNot(f.GetMustNot()); err != nil {
		return nil, false, err
	}
	must, changedMust, err := splitConditions(f.GetMust())
	if err != nil {
		return nil, false, err
	}
	should, changedShould, err := splitConditions(f.GetShould())
	if err != nil {
		return nil, false, err
	}
	if !changedMust && !changedShould {
		return []*qdrant.Filter{f}, false, nil
	}
	out := make([]*qdrant.Filter, 0, len(must)*len(should))
	for _, m := range must {
		for _, s := range should {
			// Field-by-field: copying the whole protobuf message would copy
			// its internal mutex (go vet copylocks).
			out = append(out, &qdrant.Filter{
				Must:      m,
				Should:    s,
				MustNot:   f.GetMustNot(),
				MinShould: f.GetMinShould(),
			})
		}
	}
	return out, true, nil
}

// splitConditions expands each condition into its alternatives and returns
// the cartesian product over the list, preserving condition order.
func splitConditions(conds []*qdrant.Condition) ([][]*qdrant.Condition, bool, error) {
	variants := [][]*qdrant.Condition{{}}
	changed := false
	for _, c := range conds {
		alts, ch, err := splitCondition(c)
		if err != nil {
			return nil, false, err
		}
		if ch {
			changed = true
		}
		next := make([][]*qdrant.Condition, 0, len(variants)*len(alts))
		for _, v := range variants {
			for _, a := range alts {
				nv := make([]*qdrant.Condition, len(v), len(v)+1)
				copy(nv, v)
				next = append(next, append(nv, a))
			}
		}
		variants = next
	}
	return variants, changed, nil
}

// splitCondition expands one condition: an oversized MatchAny becomes one
// condition per chunk, a nested filter is expanded recursively; a condition
// within the ceiling is returned as-is.
func splitCondition(c *qdrant.Condition) ([]*qdrant.Condition, bool, error) {
	if kw := oversizedKeywords(c); kw != nil {
		chunks := chunkStrings(kw, maxMatchAny)
		out := make([]*qdrant.Condition, 0, len(chunks))
		for _, chunk := range chunks {
			out = append(out, qdrant.NewMatchKeywords(c.GetField().GetKey(), chunk...))
		}
		return out, true, nil
	}
	if nf := c.GetFilter(); nf != nil {
		parts, changed, err := splitFilter(nf)
		if err != nil {
			return nil, false, err
		}
		if !changed {
			return []*qdrant.Condition{c}, false, nil
		}
		out := make([]*qdrant.Condition, 0, len(parts))
		for _, p := range parts {
			out = append(out, qdrant.NewFilterAsCondition(p))
		}
		return out, true, nil
	}
	return []*qdrant.Condition{c}, false, nil
}

// oversizedKeywords returns the values of a MatchAny keyword list longer than
// the per-request ceiling, or nil when the condition is not such a list.
func oversizedKeywords(c *qdrant.Condition) []string {
	kw := c.GetField().GetMatch().GetKeywords()
	if kw == nil || len(kw.GetStrings()) <= maxMatchAny {
		return nil
	}
	return kw.GetStrings()
}

// checkOversizedInMustNot rejects oversized MatchAny lists reachable through
// must_not: chunking cannot preserve exclusion semantics (a point excluded by
// one chunk could leak through another request).
func checkOversizedInMustNot(conds []*qdrant.Condition) error {
	for _, c := range conds {
		if oversizedKeywords(c) != nil {
			return fmt.Errorf("qdrant: graph scroll: MatchAny longer than %d in must_not cannot be chunked", maxMatchAny)
		}
		if nf := c.GetFilter(); nf != nil {
			if err := checkOversizedInMustNot(nf.GetMustNot()); err != nil {
				return err
			}
			if err := checkOversizedInMustNot(nf.GetMust()); err != nil {
				return err
			}
			if err := checkOversizedInMustNot(nf.GetShould()); err != nil {
				return err
			}
		}
	}
	return nil
}

// hasEmptyMatchAny reports whether a must chain contains an empty MatchAny
// (or MatchAny-integers) list. Such a condition can never be satisfied, so
// the whole filter matches nothing and no request is needed. Empty lists in
// should/must_not keep Qdrant semantics (ignored / nothing to exclude) and
// are not treated as matches-nothing.
func hasEmptyMatchAny(f *qdrant.Filter) bool {
	for _, c := range f.GetMust() {
		if fc := c.GetField(); fc != nil {
			if kw := fc.GetMatch().GetKeywords(); kw != nil && matchAnyShortCircuits(kw.GetStrings()) {
				return true
			}
			if in := fc.GetMatch().GetIntegers(); in != nil && len(in.GetIntegers()) == 0 {
				return true
			}
		}
		if nf := c.GetFilter(); nf != nil && hasEmptyMatchAny(nf) {
			return true
		}
	}
	return false
}

// chunkStrings splits values into chunks of at most size elements, preserving
// order; a non-empty input always yields at least one chunk.
func chunkStrings(values []string, size int) [][]string {
	out := make([][]string, 0, (len(values)+size-1)/size)
	for start := 0; start < len(values); start += size {
		out = append(out, values[start:min(start+size, len(values))])
	}
	return out
}

// matchAnyShortCircuits reports whether a MatchAny list can never match: an
// empty list lets ScrollAll return no points without a request.
func matchAnyShortCircuits(in []string) bool { return len(in) == 0 }
