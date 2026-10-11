package graph

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/qdrant/go-client/qdrant"
)

// Aggregation caps (plan §7.1, config MCP_GRAPH_AGG_MAX_SCANNED /
// MCP_GRAPH_AGG_MAX_GROUPS). Package state rather than parameters because the
// Aggregate signature is fixed by the mode dispatch; the MCP handler seeds it
// from configuration once at startup.
const (
	defaultAggMaxScanned = 50_000
	defaultAggMaxGroups  = 10_000
)

var aggCaps = struct{ scanned, groups int }{
	scanned: defaultAggMaxScanned,
	groups:  defaultAggMaxGroups,
}

// SetAggCaps overrides the aggregation caps from configuration; a non-positive
// value restores that cap's default.
func SetAggCaps(scanned, groups int) {
	if scanned <= 0 {
		scanned = defaultAggMaxScanned
	}
	if groups <= 0 {
		groups = defaultAggMaxGroups
	}
	aggCaps.scanned, aggCaps.groups = scanned, groups
}

// Query caps (plan §7.1, FR-025/FR-026; config MCP_GRAPH_PATH_MAX_DEPTH /
// MCP_GRAPH_MAX_EDGES_PER_NODE). Package state like aggCaps: the engine
// signatures are fixed by the mode dispatch, so the MCP handler seeds these
// from configuration once at startup.
const (
	defaultPathMaxDepth    = maxHops // 3 — the contract's server-side cap
	defaultMaxEdgesPerNode = 10_000
)

var queryCaps = struct{ pathMaxDepth, maxEdgesPerNode int }{
	pathMaxDepth:    defaultPathMaxDepth,
	maxEdgesPerNode: defaultMaxEdgesPerNode,
}

// SetQueryCaps overrides the query caps from configuration; a non-positive
// value restores that cap's default and the path depth is clamped to the
// contract bound (≤ maxHops).
func SetQueryCaps(pathMaxDepth, maxEdgesPerNode int) {
	if pathMaxDepth <= 0 {
		pathMaxDepth = defaultPathMaxDepth
	}
	if pathMaxDepth > maxHops {
		pathMaxDepth = maxHops
	}
	if maxEdgesPerNode <= 0 {
		maxEdgesPerNode = defaultMaxEdgesPerNode
	}
	queryCaps.pathMaxDepth, queryCaps.maxEdgesPerNode = pathMaxDepth, maxEdgesPerNode
}

// edgeCap truncates a per-node edge scan to the configured cap and reports the
// cut — the caller's truncation signal (an honest partial answer, never a
// silent drop).
func edgeCap(pts []*qdrant.RetrievedPoint) ([]*qdrant.RetrievedPoint, bool) {
	if len(pts) > queryCaps.maxEdgesPerNode {
		return pts[:queryCaps.maxEdgesPerNode], true
	}
	return pts, false
}

// ErrAggCapExceeded is returned when an exact aggregation (a count) would have
// to be cut by the group cap: a partial exact count would be a lie, so the
// query is refused instead. Fold aggregates (sum/avg/min/max, including counts
// over an unindexed group column) are not exact-by-contract and return a
// partial answer with Truncated set.
var ErrAggCapExceeded = errors.New("aggregation_cap_exceeded: exact aggregation exceeds the configured cap")

// Aggregator is the store surface Aggregate needs; *store.GraphStore satisfies
// it (Count=exact count, Facet=exact grouped count, ScrollAll=full scan).
type Aggregator interface {
	Count(ctx context.Context, f *qdrant.Filter) (uint64, error)
	Facet(ctx context.Context, key string, f *qdrant.Filter, limit uint64) (map[string]uint64, error)
	ScrollAll(ctx context.Context, f *qdrant.Filter, limit uint32) ([]*qdrant.RetrievedPoint, error)
}

// AggGroup is one aggregation row: the group key ("" when ungrouped), the
// point count of the group, and the op result (for count they are equal).
type AggGroup struct {
	Key   string
	Count uint64
	Value float64
}

// AggResult is the aggregation answer. Truncated reports a response cut that
// did not corrupt exactness of what is returned: the q.Limit group-list cut
// and the fold caps (partial fold values, flagged, never silent).
type AggResult struct {
	Op        string
	Groups    []AggGroup
	Truncated bool
}

// facetableFields are the keyword-indexed payload fields a grouped count can
// serve through Qdrant Facet (exact, one RTT, R2). It mirrors the keyword
// subset of store.graphIndexFields minus symbol_text (full-text); the datetime
// fields fold. Drift is performance-only: a newly indexed field just falls
// back to the fold until listed here.
var facetableFields = map[string]bool{
	"kind": true, "node_type": true, "edge_type": true, "ci_level": true,
	"repo": true, "group_org": true, "vcs_type": true, "branch": true,
	"language": true, "schema_ver": true, "symbol": true, "symbol_kind": true,
	"from_file": true, "from_package": true, "to_package": true,
	"to_package_name": true, "to_symbol": true, "file_path": true,
	"package": true, "mr_id": true, "state": true, "author": true,
	"status": true, "severity": true, "node_key": true, "from_key": true,
	"to_key": true, "from_type": true, "to_type": true, "vcs_user_id": true,
	"login": true, "name": true, "parent_key": true, "author_key": true,
	"item_id": true, "item_type": true, "ms_id": true, "label_key": true,
	"scope": true, "runner_id": true, "runner_type": true, "tags": true,
}

// Aggregate runs the aggregation family of a Query (T012, plan §7.1):
//
//   - ungrouped count uses the native exact Count — no scan cap, the server
//     counts;
//   - grouped count over one indexed keyword column uses Facet(exact) in one
//     RTT; if the group cap is hit the query is refused (ErrAggCapExceeded),
//     because an exact grouped count cannot be cut;
//   - sum/avg/min/max, unindexed/multi-column group_by, and MatchNone-free
//     fallbacks scroll once and fold. The fold caps the scan at
//     AggMaxScanned points and the distinct groups at AggMaxGroups; a cut
//     returns partial values with Truncated set;
//   - the returned group list is shaped by the compiled limit (default 30,
//     max 1000); a cut also sets Truncated.
//
// Compile validates the query first (selectivity guard included); MatchNone
// short-circuits without touching the store. An empty result is never an
// error: ungrouped aggregation returns one zero row, grouped none.
func Aggregate(ctx context.Context, store Aggregator, q Query) (AggResult, error) {
	op, err := normalizeAggOp(q.AggOp)
	if err != nil {
		return AggResult{}, err
	}
	field := strings.TrimSpace(q.AggField)
	if op != "count" && field == "" {
		return AggResult{}, fmt.Errorf("graph: aggregation op %q requires field", op)
	}
	groupFields := splitGroupBy(q.GroupBy)
	c, err := Compile(q)
	if err != nil {
		return AggResult{}, err
	}
	res := AggResult{Op: op, Groups: []AggGroup{}}
	if c.MatchNone || store == nil {
		if len(groupFields) == 0 {
			res.Groups = []AggGroup{{}}
		}
		return res, nil
	}

	if op == "count" && len(groupFields) == 0 {
		n, err := store.Count(ctx, c.Start)
		if err != nil {
			return AggResult{}, fmt.Errorf("graph: aggregation count: %w", err)
		}
		res.Groups = []AggGroup{{Count: n, Value: float64(n)}}
		return res, nil
	}

	if op == "count" && len(groupFields) == 1 && facetableFields[groupFields[0]] {
		// +1 detects a tail beyond the cap without ever materialising it.
		counts, err := store.Facet(ctx, groupFields[0], c.Start, uint64(aggCaps.groups)+1)
		if err != nil {
			return AggResult{}, fmt.Errorf("graph: aggregation facet %q: %w", groupFields[0], err)
		}
		if len(counts) > aggCaps.groups {
			return AggResult{}, fmt.Errorf("%w: groups > %d (field %q)", ErrAggCapExceeded, aggCaps.groups, groupFields[0])
		}
		groups := make([]AggGroup, 0, len(counts))
		for k, n := range counts {
			groups = append(groups, AggGroup{Key: k, Count: n, Value: float64(n)})
		}
		if err := sortAggGroups(groups, q.Sort, groupFields[0]); err != nil {
			return AggResult{}, err
		}
		res.Groups, res.Truncated = limitAggGroups(groups, c.Limit)
		return res, nil
	}

	pts, err := store.ScrollAll(ctx, c.Start, graphPageSize)
	if err != nil {
		return AggResult{}, fmt.Errorf("graph: aggregation scan: %w", err)
	}
	truncated := false
	if len(pts) > aggCaps.scanned {
		pts, truncated = pts[:aggCaps.scanned], true
	}
	groups, cut := foldGroups(op, field, groupFields, pts)
	truncated = truncated || cut
	if err := sortAggGroups(groups, q.Sort, firstGroupField(groupFields)); err != nil {
		return AggResult{}, err
	}
	res.Groups, cut = limitAggGroups(groups, c.Limit)
	res.Truncated = truncated || cut
	return res, nil
}

// normalizeAggOp validates and lower-cases the aggregation op; empty defaults
// to count.
func normalizeAggOp(op string) (string, error) {
	op = strings.ToLower(strings.TrimSpace(op))
	if op == "" {
		return "count", nil
	}
	switch op {
	case "count", "sum", "avg", "min", "max":
		return op, nil
	}
	return "", fmt.Errorf("graph: invalid aggregation op %q (want count|sum|avg|min|max)", op)
}

// splitGroupBy splits the group_by spec into trimmed, non-empty columns; more
// than one column is a composite key (fold only).
func splitGroupBy(spec string) []string {
	var out []string
	for _, f := range strings.Split(spec, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// firstGroupField is the sort-column alias of the group column (empty when
// ungrouped).
func firstGroupField(fields []string) string {
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// foldGroups scrolls-and-folds the op over pts. It returns the groups and
// whether the group cap cut the fold (partial counts, honestly flagged). An
// ungrouped fold always yields exactly one row (zeroes on an empty scan).
func foldGroups(op, field string, groupFields []string, pts []*qdrant.RetrievedPoint) ([]AggGroup, bool) {
	type acc struct {
		count, values uint64
		sum, min, max float64
	}
	accs := make(map[string]*acc)
	truncated := false
	for _, p := range pts {
		key := ""
		if len(groupFields) > 0 {
			parts := make([]string, 0, len(groupFields))
			for _, f := range groupFields {
				parts = append(parts, payloadAggKey(p.GetPayload(), f))
			}
			key = strings.Join(parts, ",")
			if _, ok := accs[key]; !ok && len(accs) >= aggCaps.groups {
				truncated = true
				break
			}
		}
		a, ok := accs[key]
		if !ok {
			a = &acc{}
			accs[key] = a
		}
		a.count++
		if op != "count" {
			if v, ok := payloadNumber(p.GetPayload(), field); ok {
				a.sum += v
				if a.values == 0 || v < a.min {
					a.min = v
				}
				if a.values == 0 || v > a.max {
					a.max = v
				}
				a.values++
			}
		}
	}
	groups := make([]AggGroup, 0, len(accs))
	for k, a := range accs {
		groups = append(groups, AggGroup{Key: k, Count: a.count, Value: accValue(op, a.count, a.sum, a.min, a.max, a.values)})
	}
	if len(groupFields) == 0 && len(groups) == 0 {
		groups = append(groups, AggGroup{})
	}
	return groups, truncated
}

// accValue reduces one accumulator to the op result.
func accValue(op string, count uint64, sum, min, max float64, values uint64) float64 {
	switch op {
	case "sum":
		return sum
	case "avg":
		if values == 0 {
			return 0
		}
		return sum / float64(values)
	case "min":
		return min
	case "max":
		return max
	default:
		return float64(count)
	}
}

// payloadAggKey renders a payload value as a group key: strings verbatim,
// numbers/bools canonically (the payload counterpart of facetValueKey).
func payloadAggKey(m map[string]*qdrant.Value, key string) string {
	v, ok := m[key]
	if !ok {
		return ""
	}
	switch v.GetKind().(type) {
	case *qdrant.Value_IntegerValue:
		return strconv.FormatInt(v.GetIntegerValue(), 10)
	case *qdrant.Value_DoubleValue:
		return strconv.FormatFloat(v.GetDoubleValue(), 'g', -1, 64)
	case *qdrant.Value_BoolValue:
		return strconv.FormatBool(v.GetBoolValue())
	default:
		return v.GetStringValue()
	}
}

// payloadNumber reads a payload field as a number for sum/avg/min/max: integer
// and double values directly, strings by parse (mr_id and friends).
func payloadNumber(m map[string]*qdrant.Value, key string) (float64, bool) {
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	switch v.GetKind().(type) {
	case *qdrant.Value_IntegerValue:
		return float64(v.GetIntegerValue()), true
	case *qdrant.Value_DoubleValue:
		return v.GetDoubleValue(), true
	case *qdrant.Value_StringValue:
		f, err := strconv.ParseFloat(v.GetStringValue(), 64)
		return f, err == nil
	}
	return 0, false
}

// sortAggGroups orders groups by the Sort spec: "" desc by value (the natural
// reading of a grouped count), "col" asc / "-col" desc where col is key, the
// group column, value or count. Ties break by key for determinism.
func sortAggGroups(groups []AggGroup, spec, groupField string) error {
	col, desc := "value", true
	if s := strings.TrimSpace(spec); s != "" {
		desc = false
		if strings.HasPrefix(s, "-") {
			s, desc = s[1:], true
		}
		switch {
		case s == "key" || (groupField != "" && s == groupField):
			col = "key"
		case s == "value" || s == "count":
			col = "value"
		default:
			return fmt.Errorf("graph: invalid aggregation sort column %q (want key|count|value)", s)
		}
	}
	sort.SliceStable(groups, func(i, j int) bool {
		a, b := groups[i], groups[j]
		if col == "key" {
			if a.Key != b.Key {
				if desc {
					return a.Key > b.Key
				}
				return a.Key < b.Key
			}
		} else if a.Value != b.Value {
			if desc {
				return a.Value > b.Value
			}
			return a.Value < b.Value
		}
		return a.Key < b.Key
	})
	return nil
}

// limitAggGroups shapes the response list by the compiled limit.
func limitAggGroups(groups []AggGroup, limit int) ([]AggGroup, bool) {
	if limit > 0 && len(groups) > limit {
		return groups[:limit], true
	}
	return groups, false
}
