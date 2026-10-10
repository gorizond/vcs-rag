//go:build e2e

package integration

import (
	"context"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gorizond/vcs-rag/internal/config"
	"github.com/gorizond/vcs-rag/internal/embed"
	"github.com/gorizond/vcs-rag/internal/gitengine"
	"github.com/gorizond/vcs-rag/internal/provider"
	"github.com/gorizond/vcs-rag/internal/store"
	"github.com/gorizond/vcs-rag/internal/worker"
)

// buildBinary builds the vcs-rag binary to a temporary path.
func buildBinary(t *testing.T, ctx context.Context) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "vcs-rag")
	build := exec.CommandContext(ctx, "go", "build", "-o", bin, "github.com/gorizond/vcs-rag/cmd/vcs-rag")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v: %s", err, out)
	}
	return bin
}

// TestE2E_Perf — T067: (a) no-change pass (repeated ProcessRepoOnce:
// timing + 0 new points) and (b) search_code latency over MCP stdio.
//
//	QDRANT_URL=http://localhost:16334 EMBEDDINGS_BASE_URL=http://localhost:11435 \
//	go test -tags e2e ./tests/integration -run TestE2E_Perf -v -count=1
func TestE2E_Perf(t *testing.T) {
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
		Collection: envOr("QDRANT_COLLECTION", "vcs_rag_e2e_perf"),
	}
	cfg.Worker = config.Worker{Interval: time.Hour, Concurrency: 2, MirrorsDir: t.TempDir(), HistoryMaxCommits: 100, MaxFileBytes: 1 << 20}

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

	src, _ := initSourceRepo(t)
	repo := provider.Repo{ID: "perf-1", Namespace: "e2e/grp", Path: "fixture", CloneURL: "file://" + src, DefaultBranch: "main"}
	deps := worker.RepoDeps{
		VCSType: "gitlab",
		Engine:  gitengine.New(cfg.Worker.MirrorsDir),
		Embed:   emb,
		Store:   st,
		Cfg:     cfg,
		Logger:  discardLogger(),
	}
	if err := worker.ProcessRepoOnce(ctx, deps, repo); err != nil {
		t.Fatalf("ProcessRepoOnce: %v", err)
	}
	count1, err := st.Count(ctx)
	if err != nil || count1 == 0 {
		t.Fatalf("count after initial indexing = %d (err=%v)", count1, err)
	}

	// (a) no-change pass: timing + no duplicates
	start := time.Now()
	if err := worker.ProcessRepoOnce(ctx, deps, repo); err != nil {
		t.Fatalf("ProcessRepoOnce (repeat): %v", err)
	}
	idle := time.Since(start)
	count2, err := st.Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count2 != count1 {
		t.Errorf("repeated pass created duplicates: %d → %d", count1, count2)
	}
	t.Logf("perf: no-change pass = %s; points %d → %d (no duplicates)",
		idle.Round(time.Millisecond), count1, count2)

	// (b) search_code latency over MCP stdio (end-to-end)
	bin := buildBinary(t, ctx)
	proc := startMCP(t, bin, []string{
		"GITLAB_GROUP_ID=1", "GITLAB_TOKEN=dummy",
		"EMBEDDINGS_PROVIDER=ollama",
		"EMBEDDINGS_BASE_URL=" + cfg.Embeddings.BaseURL,
		"QDRANT_URL=" + cfg.Qdrant.URL,
		"QDRANT_COLLECTION=" + cfg.Qdrant.Collection,
	})
	defer proc.stop(t)
	proc.call(t, 1, "initialize", `{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"perf","version":"0"}}`)
	proc.notify(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	// warm-up (model load into Ollama) — excluded from measurement
	if resp := proc.call(t, 2, "tools/call", `{"name":"search_code","arguments":{"query":"main"}}`); !strings.Contains(resp, "main.go") {
		t.Fatalf("warm-up did not return main.go: %s", truncateForLog(resp, 300))
	}

	const n = 10
	durs := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		callStart := time.Now()
		resp := proc.call(t, 10+i, "tools/call", `{"name":"search_code","arguments":{"query":"main"}}`)
		d := time.Since(callStart)
		durs = append(durs, d)
		if !strings.Contains(resp, "main.go") {
			t.Errorf("request %d did not return main.go: %s", i, truncateForLog(resp, 300))
		}
	}
	minD, maxD, sumD := durs[0], durs[0], time.Duration(0)
	for _, d := range durs {
		if d < minD {
			minD = d
		}
		if d > maxD {
			maxD = d
		}
		sumD += d
	}
	avg := sumD / time.Duration(n)
	sorted := make([]time.Duration, len(durs))
	copy(sorted, durs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	p50 := sorted[len(sorted)/2]
	t.Logf("perf: search_code latency n=%d: min=%s p50=%s avg=%s max=%s",
		n, minD.Round(time.Millisecond), p50.Round(time.Millisecond), avg.Round(time.Millisecond), maxD.Round(time.Millisecond))
	// SC-003: on a warmed-up setup a typical request is < 2 s with CPU available.
	// Spikes are possible on a shared overloaded host — the A/B control (direct HTTP
	// to Ollama) shows the same variance, see perf-baseline.md. The gate here remains
	// functional correctness of responses only (verified above).
}
