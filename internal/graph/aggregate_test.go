package graph_test

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/qdrant/go-client/qdrant"

	"github.com/gorizond/vcs-rag/internal/graph"
)

// fakeAgg — hermetic graph.Aggregator. Facet/Count/ScrollAll return canned
// results and record their calls, so the tests assert which primitive an
// aggregation picked (facet vs fold vs native count).
type fakeAgg struct {
	count      uint64
	countCalls int
	countErr   error

	facet      map[string]uint64
	facetCalls int
	facetKey   string
	facetLimit uint64
	facetErr   error

	points      []*qdrant.RetrievedPoint
	scrollCalls int
	scrollErr   error
	gotFilter   *qdrant.Filter
}

func (f *fakeAgg) Count(_ context.Context, flt *qdrant.Filter) (uint64, error) {
	f.countCalls++
	f.gotFilter = flt
	return f.count, f.countErr
}

func (f *fakeAgg) Facet(_ context.Context, key string, _ *qdrant.Filter, limit uint64) (map[string]uint64, error) {
	f.facetCalls++
	f.facetKey, f.facetLimit = key, limit
	return f.facet, f.facetErr
}

func (f *fakeAgg) ScrollAll(_ context.Context, flt *qdrant.Filter, _ uint32) ([]*qdrant.RetrievedPoint, error) {
	f.scrollCalls++
	f.gotFilter = flt
	return f.points, f.scrollErr
}

// aggPoint — one payload-only point for aggregation fixtures.
func aggPoint(id string, kv map[string]any) *qdrant.RetrievedPoint {
	return &qdrant.RetrievedPoint{Id: qdrant.NewID(id), Payload: qdrant.NewValueMap(kv)}
}

// aggQuery — an aggregation Query with the selective predicate every mode needs.
func aggQuery(op, field, groupBy string) graph.Query {
	return graph.Query{
		Mode:     graph.ModeAggregation,
		AggOp:    op,
		AggField: field,
		GroupBy:  groupBy,
		Filter:   graph.Filter{Repo: "r"},
	}
}

// TestAggregateUngroupedCount — count with no group_by is the native exact
// Count: one row, no facet, no scan.
func TestAggregateUngroupedCount(t *testing.T) {
	fake := &fakeAgg{count: 7}
	res, err := graph.Aggregate(context.Background(), fake, aggQuery("count", "", ""))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if fake.countCalls != 1 || fake.facetCalls != 0 || fake.scrollCalls != 0 {
		t.Fatalf("count=%d facet=%d scroll=%d, want native Count only", fake.countCalls, fake.facetCalls, fake.scrollCalls)
	}
	if len(res.Groups) != 1 || res.Groups[0].Key != "" || res.Groups[0].Count != 7 || res.Groups[0].Value != 7 {
		t.Fatalf("groups = %+v, want one row {key:\"\", count:7}", res.Groups)
	}
	if res.Truncated {
		t.Fatal("an exact native count must not be truncated")
	}
}

// TestAggregateFacetGroupedCount — grouped count over an indexed field goes
// through Facet(exact) with a cap-detecting limit, and sorts desc by value.
func TestAggregateFacetGroupedCount(t *testing.T) {
	graph.SetAggCaps(0, 10)
	defer graph.SetAggCaps(0, 0)
	fake := &fakeAgg{facet: map[string]uint64{"imports": 3, "defines": 5}}
	q := aggQuery("count", "", "edge_type")
	q.Limit = 10
	res, err := graph.Aggregate(context.Background(), fake, q)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if fake.facetCalls != 1 || fake.scrollCalls != 0 {
		t.Fatalf("facet=%d scroll=%d, want one Facet call", fake.facetCalls, fake.scrollCalls)
	}
	if fake.facetKey != "edge_type" || fake.facetLimit != 11 {
		t.Fatalf("facet key=%q limit=%d, want edge_type / cap+1=11", fake.facetKey, fake.facetLimit)
	}
	if len(res.Groups) != 2 || res.Groups[0].Key != "defines" || res.Groups[0].Value != 5 || res.Groups[1].Key != "imports" {
		t.Fatalf("groups = %+v, want defines(5) then imports(3)", res.Groups)
	}
}

// TestAggregateFoldOps — sum/avg/min/max scroll once and fold; an ungrouped
// fold returns exactly one row; rows without a numeric value are skipped.
func TestAggregateFoldOps(t *testing.T) {
	pts := []*qdrant.RetrievedPoint{
		aggPoint("1", map[string]any{"duration_ms": 10}),
		aggPoint("2", map[string]any{"duration_ms": 20}),
		aggPoint("3", map[string]any{"duration_ms": 30}),
		aggPoint("4", map[string]any{"other": "x"}), // no numeric value
	}
	for op, want := range map[string]float64{"sum": 60, "avg": 20, "min": 10, "max": 30} {
		fake := &fakeAgg{points: pts}
		res, err := graph.Aggregate(context.Background(), fake, aggQuery(op, "duration_ms", ""))
		if err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		if fake.scrollCalls != 1 || fake.countCalls != 0 || fake.facetCalls != 0 {
			t.Fatalf("%s: scroll=%d count=%d facet=%d, want one fold scan", op, fake.scrollCalls, fake.countCalls, fake.facetCalls)
		}
		if len(res.Groups) != 1 || res.Groups[0].Count != 4 || res.Groups[0].Value != want {
			t.Fatalf("%s: groups = %+v, want one row count=4 value=%v", op, res.Groups, want)
		}
	}
}

// TestAggregateGroupedFoldUnindexed — a count grouped by an unindexed column
// folds instead of calling Facet.
func TestAggregateGroupedFoldUnindexed(t *testing.T) {
	fake := &fakeAgg{points: []*qdrant.RetrievedPoint{
		aggPoint("1", map[string]any{"custom_col": "a"}),
		aggPoint("2", map[string]any{"custom_col": "b"}),
		aggPoint("3", map[string]any{"custom_col": "a"}),
	}}
	res, err := graph.Aggregate(context.Background(), fake, aggQuery("count", "", "custom_col"))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if fake.facetCalls != 0 || fake.scrollCalls != 1 {
		t.Fatalf("facet=%d scroll=%d, want fold", fake.facetCalls, fake.scrollCalls)
	}
	if len(res.Groups) != 2 || res.Groups[0].Key != "a" || res.Groups[0].Count != 2 || res.Groups[1].Key != "b" || res.Groups[1].Count != 1 {
		t.Fatalf("groups = %+v, want a(2) then b(1)", res.Groups)
	}
}

// TestAggregateFacetCapRefused — the exact grouped count cannot be cut: the
// group cap refuses with ErrAggCapExceeded.
func TestAggregateFacetCapRefused(t *testing.T) {
	graph.SetAggCaps(0, 2)
	defer graph.SetAggCaps(0, 0)
	fake := &fakeAgg{facet: map[string]uint64{"a": 1, "b": 1, "c": 1}}
	_, err := graph.Aggregate(context.Background(), fake, aggQuery("count", "", "state"))
	if !errors.Is(err, graph.ErrAggCapExceeded) {
		t.Fatalf("err = %v, want ErrAggCapExceeded", err)
	}
}

// TestAggregateFoldCapsTruncate — fold caps do not refuse: the partial answer
// is returned with Truncated set (scan cap and group cap).
func TestAggregateFoldCapsTruncate(t *testing.T) {
	graph.SetAggCaps(2, 0)
	defer graph.SetAggCaps(0, 0)
	fake := &fakeAgg{points: []*qdrant.RetrievedPoint{
		aggPoint("1", map[string]any{"duration_ms": 1}),
		aggPoint("2", map[string]any{"duration_ms": 2}),
		aggPoint("3", map[string]any{"duration_ms": 4}),
	}}
	res, err := graph.Aggregate(context.Background(), fake, aggQuery("sum", "duration_ms", ""))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if !res.Truncated {
		t.Fatal("scan cap must set Truncated on a fold")
	}
	if len(res.Groups) != 1 || res.Groups[0].Value != 3 {
		t.Fatalf("groups = %+v, want the first-2 sum 3", res.Groups)
	}

	graph.SetAggCaps(0, 2)
	fake = &fakeAgg{points: []*qdrant.RetrievedPoint{
		aggPoint("1", map[string]any{"custom_col": "a"}),
		aggPoint("2", map[string]any{"custom_col": "b"}),
		aggPoint("3", map[string]any{"custom_col": "c"}),
	}}
	res, err = graph.Aggregate(context.Background(), fake, aggQuery("count", "", "custom_col"))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if !res.Truncated || len(res.Groups) > 2 {
		t.Fatalf("groups = %+v truncated=%v, want ≤2 groups flagged truncated", res.Groups, res.Truncated)
	}
}

// TestAggregateLimitTruncatesGroups — the response limit shapes the group list
// and sets Truncated without an error.
func TestAggregateLimitTruncatesGroups(t *testing.T) {
	fake := &fakeAgg{facet: map[string]uint64{"a": 3, "b": 2, "c": 1}}
	q := aggQuery("count", "", "state")
	q.Limit = 2
	res, err := graph.Aggregate(context.Background(), fake, q)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if !res.Truncated || len(res.Groups) != 2 || res.Groups[0].Key != "a" || res.Groups[1].Key != "b" {
		t.Fatalf("groups = %+v truncated=%v, want top-2 flagged", res.Groups, res.Truncated)
	}
}

// TestAggregateEmptySet — an empty aggregation is zero rows, not an error.
func TestAggregateEmptySet(t *testing.T) {
	res, err := graph.Aggregate(context.Background(), &fakeAgg{}, aggQuery("count", "", ""))
	if err != nil || len(res.Groups) != 1 || res.Groups[0].Count != 0 || math.Abs(res.Groups[0].Value) != 0 {
		t.Fatalf("ungrouped count = %+v err=%v, want one zero row", res.Groups, err)
	}
	res, err = graph.Aggregate(context.Background(), &fakeAgg{}, aggQuery("count", "", "state"))
	if err != nil || len(res.Groups) != 0 {
		t.Fatalf("grouped count = %+v err=%v, want zero rows", res.Groups, err)
	}
	res, err = graph.Aggregate(context.Background(), &fakeAgg{}, aggQuery("max", "duration_ms", ""))
	if err != nil || len(res.Groups) != 1 || res.Groups[0].Count != 0 {
		t.Fatalf("ungrouped fold = %+v err=%v, want one zero row", res.Groups, err)
	}
}

// TestAggregateMatchNoneShortCircuits — a must-chain empty MatchAny matches
// nothing: no store call, zero rows (one for ungrouped).
func TestAggregateMatchNoneShortCircuits(t *testing.T) {
	q := aggQuery("count", "", "")
	q.Filter.Conditions = []*qdrant.Condition{qdrant.NewMatchKeywords("mr_id")}
	fake := &fakeAgg{count: 99}
	res, err := graph.Aggregate(context.Background(), fake, q)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if fake.countCalls != 0 || fake.facetCalls != 0 || fake.scrollCalls != 0 {
		t.Fatal("MatchNone must not touch the store")
	}
	if len(res.Groups) != 1 || res.Groups[0].Count != 0 {
		t.Fatalf("groups = %+v, want one zero row", res.Groups)
	}
}

// TestAggregateRejectsBadOp — the op vocabulary is validated.
func TestAggregateRejectsBadOp(t *testing.T) {
	if _, err := graph.Aggregate(context.Background(), &fakeAgg{}, aggQuery("median", "x", "")); err == nil {
		t.Fatal("op median must be refused")
	}
	if _, err := graph.Aggregate(context.Background(), &fakeAgg{}, aggQuery("sum", "", "")); err == nil {
		t.Fatal("sum without field must be refused")
	}
}
