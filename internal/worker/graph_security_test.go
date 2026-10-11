package worker

import (
	"context"
	"testing"

	"github.com/qdrant/go-client/qdrant"

	"github.com/gorizond/vcs-rag/internal/graph"
	"github.com/gorizond/vcs-rag/internal/provider"
)

// secStub is the security-domain provider seam of the T028 check: one
// configured findings listing. The embedded nil VCSProvider panics on any
// other method, proving securityPoints calls none of them.
type secStub struct {
	provider.VCSProvider
	findings []provider.VulnerabilityFinding
}

func (s *secStub) Type() string { return "gitlab" }

func (s *secStub) ListSecurityFindings(context.Context, provider.Repo) ([]provider.VulnerabilityFinding, error) {
	return s.findings, nil
}

// inPipelineEdges collects the (from_key, from_type, to_key, to_type) tuples of
// the in_pipeline edges of one build.
func inPipelineEdges(points []*qdrant.PointStruct) [][4]string {
	var out [][4]string
	for _, p := range points {
		m := p.GetPayload()
		if m["kind"].GetStringValue() != string(graph.KindEdge) ||
			m["edge_type"].GetStringValue() != string(graph.EdgeInPipeline) {
			continue
		}
		out = append(out, [4]string{
			m["from_key"].GetStringValue(), m["from_type"].GetStringValue(),
			m["to_key"].GetStringValue(), m["to_type"].GetStringValue(),
		})
	}
	return out
}

// TestSecurityPointsInPipeline — T028/F13: securityPoints emits the
// security_scan → ci edge (from_type=security_scan, to_type=ci) once the
// pipeline's ci point carries its canonical node_key; without that key the
// pipeline is absent from the index the caller builds and the edge is skipped,
// never fabricated.
func TestSecurityPointsInPipeline(t *testing.T) {
	ctx := context.Background()
	meta := graph.Meta{Repo: "g/r", VCSType: "gitlab", Branch: "main", SchemaVer: "4", Generation: 7}
	repo := provider.Repo{Namespace: "g", Path: "r"}
	deps := RepoDeps{VCSType: "gitlab", VCS: &secStub{findings: []provider.VulnerabilityFinding{{
		ID: "1", UUID: "u1", ScannerID: "s1", ScanID: "sc1", ScanPipelineID: "100",
	}}}, Logger: testLogger()}
	scanKey := graph.RefSecurityScan("g/r", "sc1")

	// (a) the ci point of pipeline 100 carries RefCi(...) → the edge is emitted.
	ci := graph.CiPoint(meta, graph.CiLevelJob, "100", "build", "test", "success", 0, "main", "", "")
	ciKey := ci.GetPayload()["node_key"].GetStringValue()
	points, ok := securityPoints(ctx, deps, repo, meta, "gitlab.example",
		ciPipelineKeys([]*qdrant.PointStruct{ci}), testLogger())
	if !ok {
		t.Fatal("securityPoints: ok=false, want a completed listing")
	}
	got := inPipelineEdges(points)
	if len(got) != 1 || got[0] != [4]string{scanKey, "security_scan", ciKey, "ci"} {
		t.Errorf("in_pipeline = %v, want one edge %q(security_scan) → %q(ci)", got, scanKey, ciKey)
	}
	if ciKey != "g/r|ci:job:100:build" {
		t.Errorf("ci node_key = %q, want the canonical RefCi form", ciKey)
	}

	// (b) the same finding, but the ci point carries no node_key: the pipeline
	// is absent from the index and in_pipeline is skipped, never fabricated.
	bare := &qdrant.PointStruct{
		Id: qdrant.NewID("bare-ci"),
		Payload: map[string]*qdrant.Value{
			"kind":        qdrant.NewValueString(string(graph.KindCi)),
			"pipeline_id": qdrant.NewValueString("100"),
		},
	}
	points, _ = securityPoints(ctx, deps, repo, meta, "gitlab.example",
		ciPipelineKeys([]*qdrant.PointStruct{bare}), testLogger())
	if got := inPipelineEdges(points); len(got) != 0 {
		t.Errorf("in_pipeline = %v, want none: the ci point carries no node_key", got)
	}
}
