package worker

import (
	"context"
	"testing"

	"github.com/qdrant/go-client/qdrant"

	"github.com/gorizond/vcs-rag/internal/graph"
	"github.com/gorizond/vcs-rag/internal/provider"
)

// stubReviewer is the review-family provider seam of the unit under test: a
// fixed request list and per-MR diff versions. Every other VCS method panics
// through the embedded nil interface, proving mergeRequestPoints calls none of
// them.
type stubReviewer struct {
	provider.VCSProvider
	mrs  []provider.ChangeRequest
	refs map[int64][]provider.MergeRequestDiffRef
}

func (s *stubReviewer) Type() string { return "gitlab" }

func (s *stubReviewer) ListMergeRequestNodes(context.Context, provider.Repo) ([]provider.ChangeRequest, error) {
	return s.mrs, nil
}

func (s *stubReviewer) ListMergeRequestDiffRefs(_ context.Context, _ provider.Repo, mrID int) ([]provider.MergeRequestDiffRef, error) {
	return s.refs[int64(mrID)], nil
}

// reviewEdges scans the relation points of one build by edge type.
func reviewEdges(points []*qdrant.PointStruct) map[string][][2]string {
	out := map[string][][2]string{}
	for _, p := range points {
		m := p.GetPayload()
		if m["kind"].GetStringValue() != string(graph.KindEdge) {
			continue
		}
		et := m["edge_type"].GetStringValue()
		out[et] = append(out[et], [2]string{m["from_key"].GetStringValue(), m["to_key"].GetStringValue()})
	}
	return out
}

// TestMergeRequestPointsEdgesAndSkips — US2/T025: the review nodes and edges,
// the tip-selection rule and the resolve-or-skip behaviour of has_file and the
// user joins.
func TestMergeRequestPointsEdgesAndSkips(t *testing.T) {
	ctx := context.Background()
	meta := graph.Meta{Repo: "g/r", VCSType: "gitlab", Branch: "main", SchemaVer: "3", Generation: 7}
	deps := func(mrs []provider.ChangeRequest, refs map[int64][]provider.MergeRequestDiffRef) RepoDeps {
		return RepoDeps{VCSType: "gitlab", VCS: &stubReviewer{mrs: mrs, refs: refs}, Logger: testLogger()}
	}
	repo := provider.Repo{Namespace: "g", Path: "r"}

	// (a) no merge head to match: the tip is the first listed version; the
	// legacy mr_file point carries no node_key, so has_file and the
	// username-only user joins are skipped, never fabricated.
	legacy := graph.MrFilePoint(meta, 7, provider.ChangeRequestFile{NewPath: "a.go"}, "", "open")
	mrs := []provider.ChangeRequest{{ID: 7, Title: "MR"}}
	refs := map[int64][]provider.MergeRequestDiffRef{7: {
		{Version: "42", HeadSHA: "h2", BaseSHA: "b2", StartSHA: "s2"},
		{Version: "41", HeadSHA: "h1", BaseSHA: "b1", StartSHA: "s1"},
	}}
	points := mergeRequestPoints(ctx, deps(mrs, refs), repo, meta, []*qdrant.PointStruct{legacy}, testLogger())

	kinds := map[string]int{}
	for _, p := range points {
		kinds[p.GetPayload()["kind"].GetStringValue()]++
	}
	if kinds[string(graph.KindMergeRequest)] != 1 || kinds[string(graph.KindMergeRequestDiff)] != 2 {
		t.Fatalf("node kinds = %v, want 1 merge_request + 2 merge_request_diff", kinds)
	}
	edges := reviewEdges(points)
	if len(edges[string(graph.EdgeInProject)]) != 3 {
		t.Errorf("in_project edges = %d, want 3 (mr + 2 diffs)", len(edges[string(graph.EdgeInProject)]))
	}
	if len(edges[string(graph.EdgeHasDiff)]) != 2 {
		t.Errorf("has_diff edges = %d, want 2", len(edges[string(graph.EdgeHasDiff)]))
	}
	wantTip := graph.RefMergeRequestDiff("g/r", "7", "h2")
	if latest := edges[string(graph.EdgeHasLatestDiff)]; len(latest) != 1 || latest[0][1] != wantTip {
		t.Errorf("has_latest_diff = %v, want one edge to %q (first version)", latest, wantTip)
	}
	if files := edges[string(graph.EdgeHasFile)]; len(files) != 0 {
		t.Errorf("has_file = %v, want none: the legacy file point has no node_key", files)
	}
	if joins := len(edges[string(graph.EdgeMerged)]) + len(edges[string(graph.EdgeReviewer)]); joins != 0 {
		t.Errorf("user joins = %d, want 0: usernames do not resolve to ids", joins)
	}

	// (b) the merge commit anchors the tip regardless of list order, and a
	// file point that exposes its own node_key is linked by has_file.
	mrs = []provider.ChangeRequest{{ID: 8, Merged: true, MergeCommitSHA: "h1", Reviewers: []string{"alice"}}}
	refs = map[int64][]provider.MergeRequestDiffRef{8: {
		{Version: "40", HeadSHA: "h0"},
		{Version: "41", HeadSHA: "h1"},
	}}
	keyed := &qdrant.PointStruct{
		Id: qdrant.NewID("keyed-file"),
		Payload: map[string]*qdrant.Value{
			"kind":      qdrant.NewValueString(string(graph.KindMrFile)),
			"mr_id":     qdrant.NewValueInt(8),
			"file_path": qdrant.NewValueString("b.go"),
			"node_key":  qdrant.NewValueString("g/r|mr:8|diff:h1|file:b.go"),
		},
	}
	points = mergeRequestPoints(ctx, deps(mrs, refs), repo, meta, []*qdrant.PointStruct{keyed}, testLogger())
	edges = reviewEdges(points)
	wantTip = graph.RefMergeRequestDiff("g/r", "8", "h1")
	if latest := edges[string(graph.EdgeHasLatestDiff)]; len(latest) != 1 || latest[0][1] != wantTip {
		t.Errorf("has_latest_diff = %v, want the merge-commit match %q", latest, wantTip)
	}
	if files := edges[string(graph.EdgeHasFile)]; len(files) != 1 || files[0][0] != wantTip || files[0][1] != "g/r|mr:8|diff:h1|file:b.go" {
		t.Errorf("has_file = %v, want one edge %q → keyed file", files, wantTip)
	}
}
