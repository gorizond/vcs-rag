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
//     points (file paths, repo-scoped package keys, symbols — see expandKeys);
//   - each hop scans edges matching the frontier on the direction-relevant
//     endpoints, collects them, and derives the next frontier from the far
//     endpoints, alternating files and packages/symbols;
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
	frontier := frontierKeys(start, c.Direction)

	for hop := 1; hop <= c.Hops && len(frontier) > 0; hop++ {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		keys := frontier
		if len(keys) > maxFrontier {
			keys = keys[:maxFrontier]
			res.Truncated = true
		}
		edges, err := sc.ScrollAll(ctx, hopFilter(c, keys), graphPageSize)
		if err != nil {
			return res, fmt.Errorf("graph: hop %d scan: %w", hop, err)
		}
		if len(edges) > maxEdgesPerHop {
			edges = edges[:maxEdgesPerHop]
			res.Truncated = true
		}
		collect(edges)
		frontier = frontierKeys(edges, c.Direction)
	}
	return res, nil
}

// hopFilter narrows Compiled.Edge to one hop's frontier: outgoing edges leave
// from the frontier (from_file/from_package), incoming edges arrive at it
// (to_package/to_symbol). The two endpoint fields are an OR expressed as a
// nested should condition; the store expands oversized MatchAny lists inside
// it per chunk.
func hopFilter(c Compiled, keys []string) *qdrant.Filter {
	f := &qdrant.Filter{}
	f.Must = append(f.Must, c.Edge.GetMust()...)
	if c.Direction == Incoming {
		f.Must = append(f.Must, endpointAny(keys, "to_package", "to_symbol"))
	} else {
		f.Must = append(f.Must, endpointAny(keys, "from_file", "from_package"))
	}
	return f
}

// endpointAny matches any of the given keys on any of the endpoint fields.
func endpointAny(keys []string, fields ...string) *qdrant.Condition {
	conds := make([]*qdrant.Condition, 0, len(fields))
	for _, field := range fields {
		conds = append(conds, qdrant.NewMatchKeywords(field, keys...))
	}
	return qdrant.NewFilterAsCondition(&qdrant.Filter{Should: conds})
}

// frontierKeys derives the expansion keys of a scanned point set, deduplicated
// and sorted: every hop filter is deterministic regardless of scan order.
func frontierKeys(pts []*qdrant.RetrievedPoint, dir Direction) []string {
	seen := make(map[string]bool)
	var out []string
	for _, p := range pts {
		for _, k := range expandKeys(p, dir) {
			if k == "" || seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// expandKeys maps one scanned point to the canonical endpoint values that
// continue the traversal:
//
//   - edges carry canonical endpoints directly — outgoing from_file/
//     from_package, incoming to_package/to_symbol;
//   - node-like entities (definition, mr_file, ci, vuln) carry file paths
//     (file_path, location_file) and, once a repo is known, their repo-scoped
//     package key (PkgKey — the spelling edges use for packages) plus their
//     symbol for incoming defines edges.
func expandKeys(p *qdrant.RetrievedPoint, dir Direction) []string {
	m := p.GetPayload()
	if payloadString(m, "kind") == string(KindEdge) {
		if dir == Incoming {
			return compact(payloadString(m, "to_package"), payloadString(m, "to_symbol"))
		}
		return compact(payloadString(m, "from_file"), payloadString(m, "from_package"))
	}
	if dir == Incoming {
		return compact(packageKey(m), payloadString(m, "symbol"), payloadString(m, "to_symbol"))
	}
	return compact(payloadString(m, "file_path"), payloadString(m, "location_file"),
		payloadString(m, "from_file"), packageKey(m))
}

// packageKey is the repo-scoped package key of a node-like point ("" when the
// payload carries no package or repo).
func packageKey(m map[string]*qdrant.Value) string {
	pkg, repo := payloadString(m, "package"), payloadString(m, "repo")
	if pkg == "" || repo == "" {
		return ""
	}
	return PkgKey(repo, pkg)
}

func payloadString(m map[string]*qdrant.Value, key string) string {
	if v, ok := m[key]; ok {
		return v.GetStringValue()
	}
	return ""
}

// compact drops empty strings, preserving order.
func compact(values ...string) []string {
	out := values[:0]
	for _, v := range values {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
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
