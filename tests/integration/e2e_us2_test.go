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

// envOr returns the env value or the default.
func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// TestE2E_US2 — end-to-end run against real Qdrant + Ollama.
//
// Default ports 6334/11434 are occupied by other services on the host, so
// the isolated setup runs on 16334/11435 (see smoke_us2.md):
//
//	QDRANT_URL=http://localhost:16334 EMBEDDINGS_BASE_URL=http://localhost:11435 \
//	go test -tags e2e ./tests/integration -run TestE2E_US2 -v -count=1
func TestE2E_US2(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	cfg := &config.Config{}
	cfg.Provider.Type = "gitlab" // ProcessRepoOnce does not touch provider network
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

	// clean collection
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

	// 1) first pass
	if err := worker.ProcessRepoOnce(ctx, deps, repo); err != nil {
		t.Fatalf("ProcessRepoOnce: %v", err)
	}
	count1, err := st.Count(ctx)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count1 == 0 {
		t.Fatal("count after first pass = 0")
	}
	t.Logf("read-back: count after first pass = %d", count1)

	// code point payload is complete (SC-006)
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
				t.Errorf("code point payload missing field %q: %v", key, p)
			}
		}
	}
	if !foundCode {
		t.Fatalf("no code points among payloads: %v", payloads)
	}

	// 2) repeated pass — idempotency (SC-002, FR-011)
	if err := worker.ProcessRepoOnce(ctx, deps, repo); err != nil {
		t.Fatalf("ProcessRepoOnce (repeat): %v", err)
	}
	count2, err := st.Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count2 != count1 {
		t.Errorf("idempotency violated: count %d → %d", count1, count2)
	}
	t.Logf("read-back: count after repeated pass = %d (did not grow)", count2)

	// 3) prune: delete feature.txt in the feature branch + commit
	runGit(t, src, "checkout", "feature")
	runGit(t, src, "rm", "feature.txt")
	runGit(t, src, "commit", "-m", "c5: delete feature.txt")
	runGit(t, src, "checkout", "main")
	// the mirror lane fetches new commits (WORKER_MIRROR_INTERVAL); the index
	// lane reads the local mirror only (T083).
	if _, err := deps.Engine.EnsureMirror(ctx, "gitlab", repo); err != nil {
		t.Fatalf("EnsureMirror (mirror lane): %v", err)
	}
	if err := worker.ProcessRepoOnce(ctx, deps, repo); err != nil {
		t.Fatalf("ProcessRepoOnce (prune): %v", err)
	}
	count3, err := st.Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("read-back: count after file deletion = %d", count3)
	if count3 >= count2 {
		t.Errorf("prune did not work: count %d → %d", count2, count3)
	}
	payloads, err = st.ScrollPayloads(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range payloads {
		if p["file_path"] == "feature.txt" {
			t.Errorf("points of the deleted file remain: %v", p)
		}
	}

	// 4) negative control SC-004: no working code files on disk —
	// only the mirror's git internals + sidecar state.
	mirrorPath := filepath.Join(cfg.Worker.MirrorsDir, "gitlab", "e2e_grp", "fixture.git")
	if _, err := os.Stat(gitengine.StatePath(mirrorPath)); err != nil {
		t.Errorf("state file not found: %v", err)
	}
	if _, err := os.Stat(filepath.Join(mirrorPath, "main.go")); err == nil {
		t.Error("negative control: main.go found in the mirror root (working files must not be created)")
	}
	err = filepath.WalkDir(cfg.Worker.MirrorsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if strings.HasSuffix(path, ".state.json") {
			return nil
		}
		if !strings.Contains(path, ".git"+string(filepath.Separator)) {
			t.Errorf("file outside the mirror's git internals: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
