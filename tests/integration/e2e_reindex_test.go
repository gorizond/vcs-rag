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

// TestE2E_Reindex — SC-009: несовместимая размерность отсекается ДО записи
// (0 смешанных векторов), документированный reindex (docs/reindex.md) приводит
// коллекцию к 100% векторов новой конфигурации.
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
		t.Fatalf("count до смены размерности = %d (err=%v)", count1, err)
	}

	// 1) несовместимая размерность: детерминированный отказ, count не меняется
	dimErr := st.EnsureCollection(ctx, 768)
	if dimErr == nil {
		t.Fatal("EnsureCollection(768) против коллекции 1024 должен отказать")
	}
	if !strings.Contains(dimErr.Error(), "переиндексац") {
		t.Errorf("в ошибке нет ссылки на переиндексацию: %v", dimErr)
	}
	count2, err := st.Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count2 != count1 {
		t.Errorf("count изменился при отказе: %d → %d", count1, count2)
	}
	t.Logf("read-back: отказ dim=768 при 1024: %q; count=%d (не изменился)", dimErr.Error(), count2)

	// 2) документированный reindex: drop → очистка sidecar → EnsureCollection(1024) → переиндексация
	if err := st.DropCollection(ctx); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if err := st.EnsureCollection(ctx, 1024); err != nil {
		t.Fatalf("EnsureCollection после drop: %v", err)
	}
	sidecars, _ := filepath.Glob(filepath.Join(cfg.Worker.MirrorsDir, "*", "*", "*.state.json"))
	if len(sidecars) == 0 {
		t.Fatal("sidecar *.state.json не найден")
	}
	for _, f := range sidecars {
		if err := os.Remove(f); err != nil {
			t.Fatalf("удаление sidecar %s: %v", f, err)
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
		t.Fatal("после reindex count = 0")
	}

	// 3) 100% векторов новой конфигурации (dim=1024) — сырой scroll с векторами
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
		t.Fatalf("qdrant-клиент: %v", err)
	}
	defer qc.Close()
	pts, err := qc.Scroll(ctx, &qdrant.ScrollPoints{
		CollectionName: cfg.Qdrant.Collection,
		Limit:          qdrant.PtrOf(uint32(100)),
		WithVectors:    qdrant.NewWithVectorsEnable(true),
	})
	if err != nil {
		t.Fatalf("scroll с векторами: %v", err)
	}
	if uint64(len(pts)) != count3 {
		t.Errorf("scroll вернул %d точек, count = %d", len(pts), count3)
	}
	bad := 0
	for _, p := range pts {
		// GetDenseVector учитывает и новую oneof-форму (Dense), и deprecated Data.
		if got := len(p.GetVectors().GetVector().GetDenseVector().GetData()); got != 1024 {
			bad++
			t.Errorf("вектор %s размерности %d, ожидалось 1024", p.GetId().GetUuid(), got)
		}
	}
	t.Logf("read-back: reindex: sidecar удалено %d, count=%d, векторов dim=1024: %d/%d (100%%)",
		len(sidecars), count3, len(pts)-bad, len(pts))
}
