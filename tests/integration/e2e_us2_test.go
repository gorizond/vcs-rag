//go:build e2e

package integration

import (
	"context"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
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

// envOr возвращает env или дефолт.
func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// TestE2E_US2 — сквозной прогон на реальных Qdrant + Ollama.
//
// Порты по умолчанию 6334/11434 на машине заняты чужими сервисами, поэтому
// изолированный контур поднимается на 16334/11435 (см. smoke_us2.md):
//
//	QDRANT_URL=http://localhost:16334 EMBEDDINGS_BASE_URL=http://localhost:11435 \
//	go test -tags e2e ./tests/integration -run TestE2E_US2 -v -count=1
func TestE2E_US2(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	cfg := &config.Config{}
	cfg.Provider.Type = "gitlab" // ProcessRepoOnce сеть провайдера не трогает
	cfg.Embeddings = config.Embeddings{
		Provider: envOr("EMBEDDINGS_PROVIDER", "ollama"),
		BaseURL:  envOr("EMBEDDINGS_BASE_URL", "http://localhost:11434"),
		Model:    envOr("EMBEDDINGS_MODEL", "bge-m3"),
		Dim:      1024,
		Batch:    8,
	}
	cfg.Qdrant = config.Qdrant{
		URL:        envOr("QDRANT_URL", "http://localhost:6334"),
		Collection: envOr("QDRANT_COLLECTION", "vcs_rag_e2e"),
	}
	cfg.Worker = config.Worker{
		Interval:          time.Hour,
		Concurrency:       2,
		MirrorsDir:        t.TempDir(),
		HistoryMaxCommits: 100,
		MaxFileBytes:      1 << 20,
	}

	emb, err := embed.New(cfg)
	if err != nil {
		t.Fatalf("embed.New: %v", err)
	}
	st, err := store.New(cfg.Qdrant)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer st.Close()

	// чистая коллекция
	_ = st.DropCollection(ctx)
	if err := st.EnsureCollection(ctx, uint64(cfg.Embeddings.Dim)); err != nil {
		t.Fatalf("EnsureCollection: %v", err)
	}
	if err := st.EnsureIndexes(ctx); err != nil {
		t.Fatalf("EnsureIndexes: %v", err)
	}

	src, _ := initSourceRepo(t)
	repo := provider.Repo{
		ID: "e2e-1", Namespace: "e2e/grp", Path: "fixture",
		CloneURL: "file://" + src, DefaultBranch: "main",
	}
	deps := worker.RepoDeps{
		VCSType: "gitlab",
		Engine:  gitengine.New(cfg.Worker.MirrorsDir),
		Embed:   emb,
		Store:   st,
		Cfg:     cfg,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	// 1) первый проход
	if err := worker.ProcessRepoOnce(ctx, deps, repo); err != nil {
		t.Fatalf("ProcessRepoOnce: %v", err)
	}
	count1, err := st.Count(ctx)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count1 == 0 {
		t.Fatal("count после первого прохода = 0")
	}
	t.Logf("read-back: count после первого прохода = %d", count1)

	// payload code-точки полон (SC-006)
	payloads, err := st.ScrollPayloads(ctx, 10)
	if err != nil {
		t.Fatalf("ScrollPayloads: %v", err)
	}
	required := []string{"vcs_type", "group_org", "repo", "type", "branch", "commit_sha", "file_path", "text", "chunk_index"}
	foundCode := false
	for _, p := range payloads {
		if p["type"] != "code" {
			continue
		}
		foundCode = true
		for _, key := range required {
			if _, ok := p[key]; !ok {
				t.Errorf("payload code-точки без поля %q: %v", key, p)
			}
		}
	}
	if !foundCode {
		t.Fatalf("code-точек нет среди payload: %v", payloads)
	}

	// 2) повторный проход — идемпотентность (SC-002, FR-011)
	if err := worker.ProcessRepoOnce(ctx, deps, repo); err != nil {
		t.Fatalf("ProcessRepoOnce (повтор): %v", err)
	}
	count2, err := st.Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count2 != count1 {
		t.Errorf("идемпотентность нарушена: count %d → %d", count1, count2)
	}
	t.Logf("read-back: count после повторного прохода = %d (не вырос)", count2)

	// 3) prune: удаление файла feature.txt в ветке feature + коммит
	runGit(t, src, "checkout", "feature")
	runGit(t, src, "rm", "feature.txt")
	runGit(t, src, "commit", "-m", "c5: delete feature.txt")
	runGit(t, src, "checkout", "main")
	if err := worker.ProcessRepoOnce(ctx, deps, repo); err != nil {
		t.Fatalf("ProcessRepoOnce (prune): %v", err)
	}
	count3, err := st.Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("read-back: count после удаления файла = %d", count3)
	if count3 >= count2 {
		t.Errorf("prune не сработал: count %d → %d", count2, count3)
	}
	payloads, err = st.ScrollPayloads(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range payloads {
		if p["file_path"] == "feature.txt" {
			t.Errorf("точки удалённого файла остались: %v", p)
		}
	}

	// 4) негативный контроль SC-004: рабочих файлов кода на диске нет —
	// только git-интерналы зеркала + sidecar-состояние.
	mirrorPath := filepath.Join(cfg.Worker.MirrorsDir, "gitlab", "e2e_grp", "fixture.git")
	if _, err := os.Stat(gitengine.StatePath(mirrorPath)); err != nil {
		t.Errorf("state-файл не найден: %v", err)
	}
	if _, err := os.Stat(filepath.Join(mirrorPath, "main.go")); err == nil {
		t.Error("негативный контроль: main.go найден в корне зеркала (рабочие файлы не должны создаваться)")
	}
	err = filepath.WalkDir(cfg.Worker.MirrorsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if strings.HasSuffix(path, ".state.json") {
			return nil
		}
		if !strings.Contains(path, ".git"+string(filepath.Separator)) {
			t.Errorf("файл вне git-интерналов зеркала: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
