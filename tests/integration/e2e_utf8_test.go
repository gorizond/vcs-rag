//go:build e2e

package integration

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gorizond/vcs-rag/internal/config"
	"github.com/gorizond/vcs-rag/internal/embed"
	"github.com/gorizond/vcs-rag/internal/gitengine"
	"github.com/gorizond/vcs-rag/internal/provider"
	"github.com/gorizond/vcs-rag/internal/store"
	"github.com/gorizond/vcs-rag/internal/worker"
)

// T158: a file with invalid UTF-8 must not panic the indexing pass — the
// content is stored with U+FFFD replacements and the repository is indexed.
// Before the fix the Qdrant client panicked ("invalid UTF-8 in string") on the
// payload upsert, so such a repository stayed pending forever.
func TestE2E_InvalidUTF8(t *testing.T) {
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
		Collection: envOr("QDRANT_COLLECTION", "vcs_rag_e2e_utf8"),
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
	_ = st.DropCollection(ctx)
	if err := st.EnsureCollection(ctx, uint64(cfg.Embeddings.Dim)); err != nil {
		t.Fatalf("EnsureCollection: %v", err)
	}
	if err := st.EnsureIndexes(ctx); err != nil {
		t.Fatalf("EnsureIndexes: %v", err)
	}

	// a fixture with a source file whose bytes are not valid UTF-8.
	src := t.TempDir()
	runGit(t, src, "init", "-b", "main")
	writeFile(t, src, "main.go", "package main\n\nfunc main() {}\n")
	badPath := filepath.Join(src, "broken.go")
	if err := os.WriteFile(badPath, []byte("package main\n\n// \x88\xac\xef broken bytes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, src, "add", ".")
	runGit(t, src, "commit", "-m", "c1: invalid utf-8 fixture")

	repo := provider.Repo{
		ID: "e2e-utf8", Namespace: "e2e/grp", Path: "utf8-fixture",
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

	// the pass completes without a panic (the pre-T158 behavior panicked here).
	if err := worker.ProcessRepoOnce(ctx, deps, repo); err != nil {
		t.Fatalf("ProcessRepoOnce: %v", err)
	}

	// the repository is indexed: at least one branch carries indexed commits.
	statePath := gitengine.StatePath(deps.Engine.MirrorPath("gitlab", repo))
	state, err := worker.LoadState(statePath)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	indexed := 0
	for _, b := range state.Branches {
		indexed += len(b.IndexedCommits)
	}
	if indexed == 0 {
		t.Fatalf("indexed_commits = 0, want the repository indexed: %+v", state.Branches)
	}
	t.Logf("read-back: indexed_commits = %d", indexed)

	// the stored text is valid UTF-8 with U+FFFD replacements.
	payloads, err := st.ScrollPayloads(ctx, 20)
	if err != nil {
		t.Fatalf("ScrollPayloads: %v", err)
	}
	found := false
	for _, p := range payloads {
		if p["file_path"] != "broken.go" {
			continue
		}
		found = true
		text, _ := p["text"].(string)
		if !utf8.ValidString(text) {
			t.Errorf("stored text is not valid UTF-8: %q", text)
		}
		if !strings.ContainsRune(text, '\uFFFD') {
			t.Errorf("stored text has no U+FFFD replacement: %q", text)
		}
	}
	if !found {
		t.Fatalf("no payload for broken.go: %v", payloads)
	}
}
