package graph

import (
	"context"
	"fmt"
	"sort"

	"github.com/qdrant/go-client/qdrant"
)

// Per-hop traversal caps (T038, research R5): a hot package ("utils") can fan
// out to hundreds of importers, so each hop is bounded. Overflow never fails a
// query — it sets Result.Truncated, the honest signal for callers.
const (
	maxFrontier    = 10000 // frontier keys expanded per hop
	maxEdgesPerHop = 20000 // edge points scanned per hop
	graphPageSize  = uint32(1000)
)

// Scroller is the graph-store read surface Traverse needs; *store.GraphStore
// satisfies it. Keeping it an interface makes the traversal pure logic over an
// injected scanner (and avoids importing the store).
type Scroller interface {
	ScrollAll(ctx context.Context, f *qdrant.Filter, limit uint32) ([]*qdrant.RetrievedPoint, error)
}

// Result is one traversal answer: the deduplicated points (start points first,
// then hop edges, capped at Query.Limit), the coverage the caller passed in,
// and whether any cap cut the traversal short.
type Result struct {
	Points    []*qdrant.RetrievedPoint
	Coverage  Coverage
	Truncated bool
}

// Traverse runs the iterative N-hop walk of a Query over sc (T038):
//
//   - start scan: Compiled.Start; the frontier keys are derived from the start
//     points (canonical refs, file paths, bare package names, symbols — see
//     frontierKeys);
//   - each hop scans edges matching the frontier on the direction-relevant
//     endpoint fields (canonical from_key/to_key plus the legacy spellings),
//     collects them, and derives the next frontier from the scanned points;
//   - caps: frontier ≤10000 keys/hop, edges ≤20000 scanned/hop, results ≤
//     Limit — any cut sets Truncated. Points are deduplicated by ID across all
//     scans.
//
// Compile errors (including ErrNonSelective) are returned as-is; a MatchNone
// query returns an empty result without touching the Scroller.
func Traverse(ctx context.Context, sc Scroller, q Query, coverage Coverage) (Result, error) {
	res := Result{Points: []*qdrant.RetrievedPoint{}, Coverage: coverage}
	c, err := Compile(q)
	if err != nil {
		return Result{}, err
	}
	if c.MatchNone || sc == nil {
		return res, nil
	}

	seen := make(map[string]bool)
	collect := func(pts []*qdrant.RetrievedPoint) {
		for _, p := range pts {
			key := pointIDKey(p.GetId())
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			if len(res.Points) >= c.Limit {
				res.Truncated = true
				continue
			}
			res.Points = append(res.Points, p)
		}
	}

	start, err := sc.ScrollAll(ctx, c.Start, graphPageSize)
	if err != nil {
		return res, fmt.Errorf("graph: start scan: %w", err)
	}
	collect(start)

	fr := frontierKeys(start, c.Direction)
	for hop := 1; hop <= c.Hops && !fr.empty(); hop++ {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		var cut bool
		if fr, cut = fr.clamp(maxFrontier); cut {
			res.Truncated = true
		}
		flt := hopFilter(c, fr)
		if flt == nil {
			break // no endpoint of this direction: the walk is complete
		}
		edges, err := sc.ScrollAll(ctx, flt, graphPageSize)
		if err != nil {
			return res, fmt.Errorf("graph: hop %d scan: %w", hop, err)
		}
		if len(edges) > maxEdgesPerHop {
			edges = edges[:maxEdgesPerHop]
			res.Truncated = true
		}
		collect(edges)
		fr = frontierKeys(edges, c.Direction)
	}
	return res, nil
}

// frontier is the typed expansion state of one traversal step. Canonical
// endpoint refs (node_key on nodes, from_key/to_key on edges — the same string
// by construction) are one family of their own: the orbit edge families
// (authored/commented/assigned_to/labelled/in_milestone/executed_by) carry
// only them, while code edges also keep the legacy spellings. File paths and
// the two package key forms are kept apart so each hop matches only its own
// endpoint fields: a mixed key list on both fields lets the package key derived
// from one file pull in every edge of the whole package (US5: 616 edges instead
// of that file's 14).
type frontier struct {
	files    []string // legacy file paths (file_path, location_file, from_file)
	canon    []string // canonical refs (node_key ↔ edge from_key/to_key)
	pkgKeys  []string // repo-namespaced package keys (PkgKey, edge to_package)
	pkgNames []string // bare package names (definition `package` ↔ to_package_name)
	syms     []string // symbols (symbol, to_symbol)
}

// empty reports whether the frontier can no longer continue the traversal.
func (fr frontier) empty() bool {
	return len(fr.files) == 0 && len(fr.canon) == 0 && len(fr.pkgKeys) == 0 && len(fr.pkgNames) == 0 && len(fr.syms) == 0
}

// clamp bounds every key set to max keys; true means a cut happened.
func (fr frontier) clamp(max int) (frontier, bool) {
	cut := false
	if len(fr.files) > max {
		fr.files, cut = fr.files[:max], true
	}
	if len(fr.canon) > max {
		fr.canon, cut = fr.canon[:max], true
	}
	if len(fr.pkgKeys) > max {
		fr.pkgKeys, cut = fr.pkgKeys[:max], true
	}
	if len(fr.pkgNames) > max {
		fr.pkgNames, cut = fr.pkgNames[:max], true
	}
	if len(fr.syms) > max {
		fr.syms, cut = fr.syms[:max], true
	}
	return fr, cut
}

// endpointKeys is one (field, keys) OR-branch of a hop filter.
type endpointKeys struct {
	field string
	keys  []string
}

// hopFilter narrows Compiled.Edge to one hop's frontier. Each direction matches
// only the endpoint fields it traverses, canonical first with the legacy
// spellings OR-ed in behind it (T049): outgoing departs via from_key (orbit
// edges) or from_file (code edges); incoming arrives via to_key (orbit edges)
// or the legacy to_package / to_package_name / to_symbol (code edges). Legacy
// keys stay in the OR for points written before canonical keys existed; they
// never widen beyond their old granularity. A file's package key is never
// matched on the outgoing side: it would widen the scan to the whole package
// (US5). nil means the frontier has no endpoint of this direction.
func hopFilter(c Compiled, fr frontier) *qdrant.Filter {
	f := &qdrant.Filter{}
	f.Must = append(f.Must, c.Edge.GetMust()...)
	var cond *qdrant.Condition
	if c.Direction == Incoming {
		cond = endpointOr(
			endpointKeys{"to_key", fr.canon},
			endpointKeys{"to_package", fr.pkgKeys},
			endpointKeys{"to_package_name", fr.pkgNames},
			endpointKeys{"to_symbol", fr.syms},
		)
	} else {
		cond = endpointOr(
			endpointKeys{"from_key", fr.canon},
			endpointKeys{"from_file", fr.files},
		)
	}
	if cond == nil {
		return nil
	}
	f.Must = append(f.Must, cond)
	return f
}

// endpointOr ORs one MatchAny condition per non-empty (field, keys) pair; nil
// when every pair is empty.
func endpointOr(pairs ...endpointKeys) *qdrant.Condition {
	conds := make([]*qdrant.Condition, 0, len(pairs))
	for _, p := range pairs {
		if len(p.keys) == 0 {
			continue
		}
		conds = append(conds, qdrant.NewMatchKeywords(p.field, p.keys...))
	}
	if len(conds) == 0 {
		return nil
	}
	return qdrant.NewFilterAsCondition(&qdrant.Filter{Should: conds})
}

// frontierKeys derives the typed expansion keys of a scanned point set,
// deduplicated and sorted (deterministic filters regardless of scan order):
//
//   - canonical keys come from every point: node_key on nodes, from_key
//     (outgoing) or to_key (incoming) on edges — this is what carries the
//     orbit edge families, whose endpoints have no legacy spellings;
//   - an outgoing hop continues from files only (from_file): a file start must
//     not widen to its package (US5) — package-level lookups belong to
//     get_imports/get_dependents;
//   - an incoming hop continues from packages and symbols: edge-derived
//     to_package keeps the full key, node-derived `package` uses the bare name
//     (to_package_name, the spelling edges carry), symbols feed defines edges.
func frontierKeys(pts []*qdrant.RetrievedPoint, dir Direction) frontier {
	var fr frontier
	seen := make(map[string]bool)
	add := func(dst *[]string, family, v string) {
		if v == "" || seen[family+"\x00"+v] {
			return
		}
		seen[family+"\x00"+v] = true
		*dst = append(*dst, v)
	}
	for _, p := range pts {
		m := p.GetPayload()
		if payloadString(m, "kind") == string(KindEdge) {
			if dir == Incoming {
				add(&fr.pkgKeys, "pkgkey", payloadString(m, "to_package"))
				add(&fr.syms, "sym", payloadString(m, "to_symbol"))
				add(&fr.canon, "canon", payloadString(m, "to_key"))
			} else {
				add(&fr.files, "file", payloadString(m, "from_file"))
				add(&fr.canon, "canon", payloadString(m, "from_key"))
			}
			continue
		}
		// Nodes: node_key is the endpoint both directions resolve to (edge
		// from_key/to_key equal it by construction).
		add(&fr.canon, "canon", payloadString(m, "node_key"))
		if dir == Incoming {
			add(&fr.pkgNames, "pkgname", payloadString(m, "package"))
			add(&fr.syms, "sym", payloadString(m, "symbol"))
			add(&fr.syms, "sym", payloadString(m, "to_symbol"))
		} else {
			add(&fr.files, "file", payloadString(m, "file_path"))
			add(&fr.files, "file", payloadString(m, "location_file"))
			add(&fr.files, "file", payloadString(m, "from_file"))
		}
	}
	sort.Strings(fr.files)
	sort.Strings(fr.canon)
	sort.Strings(fr.pkgKeys)
	sort.Strings(fr.pkgNames)
	sort.Strings(fr.syms)
	return fr
}

// payloadString reads a string payload field.
func payloadString(m map[string]*qdrant.Value, key string) string {
	if v, ok := m[key]; ok {
		return v.GetStringValue()
	}
	return ""
}

// pointIDKey renders a point ID for deduplication: canonical UUIDs compare as
// strings, numeric IDs are zero-padded to compare numerically.
func pointIDKey(id *qdrant.PointId) string {
	switch {
	case id == nil:
		return ""
	case id.GetUuid() != "":
		return "u:" + id.GetUuid()
	default:
		return fmt.Sprintf("n:%020d", id.GetNum())
	}
}
