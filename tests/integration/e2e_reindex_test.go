//go:build e2e

package integration

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/qdrant/go-client/qdrant"

	"github.com/gorizond/vcs-rag/internal/config"
	"github.com/gorizond/vcs-rag/internal/embed"
	"github.com/gorizond/vcs-rag/internal/gitengine"
	"github.com/gorizond/vcs-rag/internal/provider"
	"github.com/gorizond/vcs-rag/internal/store"
	"github.com/gorizond/vcs-rag/internal/worker"
)

// TestE2E_Reindex — SC-009: incompatible dimension is rejected BEFORE any write
// (0 mixed vectors), the documented reindex (docs/reindex.md) brings
// the collection to 100% vectors of the new configuration.
//
//	QDRANT_URL=http://localhost:16334 EMBEDDINGS_BASE_URL=http://localhost:11435 \
//	go test -tags e2e ./tests/integration -run TestE2E_Reindex -v -count=1
func TestE2E_Reindex(t *testing.T) {
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
		Collection: envOr("QDRANT_COLLECTION", "vcs_rag_e2e_reindex"),
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
	if err := st.EnsureCollection(ctx, 1024); err != nil {
		t.Fatalf("EnsureCollection(1024): %v", err)
	}

	src, _ := initSourceRepo(t)
	repo := provider.Repo{ID: "reindex-1", Namespace: "e2e/grp", Path: "fixture", CloneURL: "file://" + src, DefaultBranch: "main"}
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
		t.Fatalf("count before dimension change = %d (err=%v)", count1, err)
	}

	// 1) incompatible dimension: deterministic failure, count unchanged
	dimErr := st.EnsureCollection(ctx, 768)
	if dimErr == nil {
		t.Fatal("EnsureCollection(768) against a 1024 collection must fail")
	}
	if !strings.Contains(dimErr.Error(), "reindex") {
		t.Errorf("error does not reference reindex: %v", dimErr)
	}
	count2, err := st.Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count2 != count1 {
		t.Errorf("count changed on failure: %d → %d", count1, count2)
	}
	t.Logf("read-back: dim=768 rejected at 1024: %q; count=%d (unchanged)", dimErr.Error(), count2)

	// 2) documented reindex: drop → sidecar cleanup → EnsureCollection(1024) → reindex
	if err := st.DropCollection(ctx); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if err := st.EnsureCollection(ctx, 1024); err != nil {
		t.Fatalf("EnsureCollection after drop: %v", err)
	}
	sidecars, _ := filepath.Glob(filepath.Join(cfg.Worker.MirrorsDir, "*", "*", "*.state.json"))
	if len(sidecars) == 0 {
		t.Fatal("sidecar *.state.json not found")
	}
	for _, f := range sidecars {
		if err := os.Remove(f); err != nil {
			t.Fatalf("removing sidecar %s: %v", f, err)
		}
	}
	if err := worker.ProcessRepoOnce(ctx, deps, repo); err != nil {
		t.Fatalf("ProcessRepoOnce (reindex): %v", err)
	}
	count3, err := st.Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count3 == 0 {
		t.Fatal("count after reindex = 0")
	}

	// 3) 100% vectors of the new configuration (dim=1024) — raw scroll with vectors
	u, err := url.Parse(cfg.Qdrant.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	qc, err := qdrant.NewClient(&qdrant.Config{Host: u.Hostname(), Port: port, SkipCompatibilityCheck: true})
	if err != nil {
		t.Fatalf("qdrant client: %v", err)
	}
	defer qc.Close()
	pts, err := qc.Scroll(ctx, &qdrant.ScrollPoints{
		CollectionName: cfg.Qdrant.Collection,
		Limit:          qdrant.PtrOf(uint32(100)),
		WithVectors:    qdrant.NewWithVectorsEnable(true),
	})
	if err != nil {
		t.Fatalf("scroll with vectors: %v", err)
	}
	if uint64(len(pts)) != count3 {
		t.Errorf("scroll returned %d points, count = %d", len(pts), count3)
	}
	bad := 0
	for _, p := range pts {
		// GetDenseVector handles both the new oneof form (Dense) and deprecated Data.
		if got := len(p.GetVectors().GetVector().GetDenseVector().GetData()); got != 1024 {
			bad++
			t.Errorf("vector %s has dimension %d, expected 1024", p.GetId().GetUuid(), got)
		}
	}
	t.Logf("read-back: reindex: sidecars removed %d, count=%d, dim=1024 vectors: %d/%d (100%%)",
		len(sidecars), count3, len(pts)-bad, len(pts))
}
