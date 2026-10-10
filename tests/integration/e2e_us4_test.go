//go:build e2e

package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gorizond/vcs-rag/internal/config"
	"github.com/gorizond/vcs-rag/internal/embed"
	"github.com/gorizond/vcs-rag/internal/gitengine"
	"github.com/gorizond/vcs-rag/internal/provider"
	"github.com/gorizond/vcs-rag/internal/provider/fake"
	"github.com/gorizond/vcs-rag/internal/store"
	"github.com/gorizond/vcs-rag/internal/worker"
)

// TestE2E_US4 — metadata (issues/MR/CI) on real Qdrant+Ollama:
// fake provider → ProcessMetadataOnce → read-back, idempotency, prune.
//
// The setup is isolated (16334/11435; commands in smoke_us2.md):
//
//	QDRANT_URL=http://localhost:16334 EMBEDDINGS_BASE_URL=http://localhost:11435 \
//	go test -tags e2e ./tests/integration -run TestE2E_US4 -v -count=1
func TestE2E_US4(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	cfg := &config.Config{}
	cfg.Provider.Type = "gitlab"
	cfg.Embeddings = config.Embeddings{
		Provider: envOr("EMBEDDINGS_PROVIDER", "ollama"),
		BaseURL:  envOr("EMBEDDINGS_BASE_URL", "http://localhost:11434"),
		Model:    envOr("EMBEDDINGS_MODEL", "bge-m3"),
		Dim:      1024,
		Batch:    8,
	}
	cfg.Qdrant = config.Qdrant{
		URL:        envOr("QDRANT_URL", "http://localhost:6334"),
		Collection: envOr("QDRANT_COLLECTION", "vcs_rag_e2e4"),
	}
	cfg.Worker = config.Worker{Interval: time.Hour, Concurrency: 2, MirrorsDir: t.TempDir()}

	emb, err := embed.New(cfg)
	if err != nil {
		t.Fatalf("embed.New: %v", err)
	}
	st, err := store.New(cfg.Qdrant)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer st.Close()
	_ = st.DropCollection(ctx)
	if err := st.EnsureCollection(ctx, uint64(cfg.Embeddings.Dim)); err != nil {
		t.Fatalf("EnsureCollection: %v", err)
	}
	if err := st.EnsureIndexes(ctx); err != nil {
		t.Fatalf("EnsureIndexes: %v", err)
	}

	repo := provider.Repo{ID: "1", Namespace: "e2e/grp", Path: "fixture", CloneURL: "https://example.com/fixture.git"}
	vcs := newFakeRepo()
	deps := worker.RepoDeps{
		VCSType: "gitlab",
		VCS:     vcs,
		Engine:  gitengine.New(t.TempDir()),
		Embed:   emb,
		Store:   st,
		Cfg:     cfg,
		Logger:  discardLogger(),
	}

	// 1) first pass: issue + PR + failed CI
	if err := worker.ProcessMetadataOnce(ctx, deps, repo); err != nil {
		t.Fatalf("ProcessMetadataOnce: %v", err)
	}
	payloads := scrollPayloads(t, st, ctx)
	counts := classifyPayloads(payloads)
	t.Logf("read-back: issue=%d pull_request=%d ci_failure=%d (total %d)",
		counts["issue"], counts["pull_request"], counts["ci_failure"], len(payloads))
	if counts["issue"] != 1 || counts["pull_request"] != 1 || counts["ci_failure"] != 1 {
		t.Fatalf("counts = %v, expected 1 of each type", counts)
	}

	checkMetaPayloads(t, payloads)

	// 2) idempotency: repeated pass adds no points
	if err := worker.ProcessMetadataOnce(ctx, deps, repo); err != nil {
		t.Fatalf("ProcessMetadataOnce (repeat): %v", err)
	}
	again := scrollPayloads(t, st, ctx)
	if len(again) != len(payloads) {
		t.Errorf("idempotency: points %d → %d", len(payloads), len(again))
	}
	t.Logf("read-back: repeated pass — points %d (did not grow)", len(again))

	// 3) prune: pipeline rerun as success → ci point removed
	vcs.Pipelines["1"] = []provider.Pipeline{{ID: "100", Status: "success", Ref: "main", SHA: "abc"}}
	if err := worker.ProcessMetadataOnce(ctx, deps, repo); err != nil {
		t.Fatalf("ProcessMetadataOnce (prune): %v", err)
	}
	final := scrollPayloads(t, st, ctx)
	finalCounts := classifyPayloads(final)
	t.Logf("read-back: after success — issue=%d pull_request=%d ci_failure=%d",
		finalCounts["issue"], finalCounts["pull_request"], finalCounts["ci_failure"])
	if finalCounts["ci_failure"] != 0 {
		t.Errorf("prune did not work: ci_failure = %d", finalCounts["ci_failure"])
	}
	if finalCounts["issue"] != 1 || finalCounts["pull_request"] != 1 {
		t.Errorf("prune affected issues/PR: %v", finalCounts)
	}
}

// newFakeRepo — fake provider: 1 issue, 1 PR, 1 failed pipeline with a 100-line log.
func newFakeRepo() *fake.Provider {
	var log strings.Builder
	for i := 1; i <= 100; i++ {
		fmt.Fprintf(&log, "line %d\n", i)
	}
	return &fake.Provider{
		Repos: []provider.Repo{{ID: "1", Namespace: "e2e/grp", Path: "fixture"}},
		Issues: map[string][]provider.Issue{
			"1": {{ID: 1, Title: "Issue one", Body: "issue body", State: "opened"}},
		},
		ChangeRequests: map[string][]provider.ChangeRequest{
			"1": {{ID: 2, Title: "PR one", Body: "pr body", State: "opened", SourceBranch: "feat", TargetBranch: "main"}},
		},
		Pipelines: map[string][]provider.Pipeline{
			"1": {{ID: "100", Status: "failed", Ref: "main", SHA: "abc",
				Jobs: []provider.Job{{ID: "1000", Name: "build", Status: "failed"}}}},
		},
		Logs: map[string]string{"1000": log.String()},
	}
}

// scrollPayloads reads all points of the collection.
func scrollPayloads(t *testing.T, st *store.Store, ctx context.Context) []map[string]any {
	t.Helper()
	payloads, err := st.ScrollPayloads(ctx, 100)
	if err != nil {
		t.Fatalf("ScrollPayloads: %v", err)
	}
	return payloads
}

// classifyPayloads counts points by the type field.
func classifyPayloads(payloads []map[string]any) map[string]int {
	counts := make(map[string]int)
	for _, p := range payloads {
		if typ, ok := p["type"].(string); ok {
			counts[typ]++
		}
	}
	return counts
}

// checkMetaPayloads verifies payload completeness and the CI log tail.
func checkMetaPayloads(t *testing.T, payloads []map[string]any) {
	t.Helper()
	required := map[string][]string{
		"issue":        {"vcs_type", "group_org", "repo", "type", "issue_id", "state", "text"},
		"pull_request": {"vcs_type", "group_org", "repo", "type", "pr_id", "source_branch", "target_branch", "state", "text"},
		"ci_failure":   {"vcs_type", "group_org", "repo", "type", "pipeline_id", "job_id", "job_name", "branch", "commit_sha", "text"},
	}
	for _, p := range payloads {
		typ, _ := p["type"].(string)
		for _, key := range required[typ] {
			if _, ok := p[key]; !ok {
				t.Errorf("payload type=%s missing field %q: %v", typ, key, p)
			}
		}
		if typ != "ci_failure" {
			continue
		}
		text, _ := p["text"].(string)
		lines := strings.Split(text, "\n")
		if len(lines) > 1+50 {
			t.Errorf("ci text: %d lines, expected ≤ 51 (header + 50)", len(lines))
		}
		if !strings.HasSuffix(text, "line 100") {
			t.Errorf("ci text does not contain the log tail: %q", text)
		}
		if strings.Contains(text, "line 50") {
			t.Errorf("tail contains a line outside the last 50: %q", text)
		}
		if !strings.Contains(lines[0], "build") {
			t.Errorf("ci text header = %q", lines[0])
		}
	}
}
