//go:build e2e

package integration

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
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

// discardLogger — a quiet logger for e2e.
func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestE2E_US3 — end-to-end MCP search run: fixture indexing (as in US2) →
// `-mode=mcp` subprocess (stdio) → JSON-RPC initialize/tools/list/tools/call
// search_code → structured error when Qdrant is unavailable.
//
// The setup is isolated (16334/11435; ports 6334/11434 are occupied by other
// services), commands are in smoke_us2.md / smoke_us3.md:
//
//	QDRANT_URL=http://localhost:16334 EMBEDDINGS_BASE_URL=http://localhost:11435 \
//	go test -tags e2e ./tests/integration -run TestE2E_US3 -v -count=1
func TestE2E_US3(t *testing.T) {
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
		Collection: envOr("QDRANT_COLLECTION", "vcs_rag_e2e3"),
	}
	cfg.Worker = config.Worker{
		Interval:          time.Hour,
		Concurrency:       2,
		MirrorsDir:        t.TempDir(),
		HistoryMaxCommits: 100,
		MaxFileBytes:      1 << 20,
	}

	// 1) fixture indexing (following e2e_us2)
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
	repo := provider.Repo{
		ID: "e2e3", Namespace: "e2e/grp", Path: "fixture",
		CloneURL: "file://" + src, DefaultBranch: "main",
	}
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
	count, err := st.Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("read-back: indexed points = %d", count)
	if count == 0 {
		t.Fatal("count = 0")
	}

	// 2) build the binary
	bin := filepath.Join(t.TempDir(), "vcs-rag")
	build := exec.CommandContext(ctx, "go", "build", "-o", bin, "github.com/gorizond/vcs-rag/cmd/vcs-rag")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v: %s", err, out)
	}

	baseEnv := []string{
		"GITLAB_GROUP_ID=1", "GITLAB_TOKEN=dummy",
		"EMBEDDINGS_PROVIDER=ollama",
		"EMBEDDINGS_BASE_URL=" + cfg.Embeddings.BaseURL,
		"QDRANT_COLLECTION=" + cfg.Qdrant.Collection,
	}
	initParams := `{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"e2e","version":"0"}}`

	// 3) working setup: stdio + real Qdrant/Ollama
	proc := startMCP(t, bin, append(append([]string{}, baseEnv...), "QDRANT_URL="+cfg.Qdrant.URL))
	defer proc.stop(t)

	if resp := proc.call(t, 1, "initialize", initParams); !strings.Contains(resp, `"vcs-rag"`) {
		t.Errorf("initialize without serverInfo: %s", resp)
	}
	proc.notify(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	toolsResp := proc.call(t, 2, "tools/list", `{}`)
	for _, name := range []string{"search_code", "search_pull_requests", "search_issues", "search_ci_failures"} {
		if !strings.Contains(toolsResp, name) {
			t.Errorf("tools/list missing %s: %s", name, truncateForLog(toolsResp, 300))
		}
	}

	searchResp := proc.call(t, 3, "tools/call", `{"name":"search_code","arguments":{"query":"main"}}`)
	t.Logf("read-back: search_code = %s", truncateForLog(searchResp, 700))
	if !strings.Contains(searchResp, "fixture") {
		t.Errorf("search_code response missing fixture: %s", truncateForLog(searchResp, 500))
	}
	if !strings.Contains(searchResp, "main.go") {
		t.Errorf("search_code response missing main.go: %s", truncateForLog(searchResp, 500))
	}

	// 4) negative: Qdrant unavailable — structured error, process stays alive
	badEnv := append(append([]string{}, baseEnv...), "QDRANT_URL=http://127.0.0.1:19999")
	bad := startMCP(t, bin, badEnv)
	defer bad.stop(t)

	bad.call(t, 1, "initialize", initParams)
	bad.notify(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	errResp := bad.call(t, 2, "tools/call", `{"name":"search_code","arguments":{"query":"main"}}`)
	t.Logf("read-back: search_code with Qdrant unavailable = %s", truncateForLog(errResp, 300))
	if !strings.Contains(errResp, "qdrant_unavailable") {
		t.Errorf("no structured error: %s", truncateForLog(errResp, 400))
	}
	alive := bad.call(t, 3, "tools/list", `{}`)
	if !strings.Contains(alive, "search_code") {
		t.Errorf("process is not responding after the error: %s", truncateForLog(alive, 300))
	}
}

// truncateForLog truncates a string for logging.
func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// mcpProcess — stdio subprocess of the MCP server.
type mcpProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	lines  chan string
	stderr *os.File
}

// startMCP starts the binary in mcp mode and reads stdout line by line.
func startMCP(t *testing.T, bin string, extraEnv []string) *mcpProcess {
	t.Helper()
	cmd := exec.Command(bin, "-mode=mcp")
	cmd.Env = append(os.Environ(), extraEnv...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout: %v", err)
	}
	stderrFile, err := os.CreateTemp(t.TempDir(), "mcp-stderr-*.log")
	if err != nil {
		t.Fatalf("stderr: %v", err)
	}
	cmd.Stderr = stderrFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", bin, err)
	}
	lines := make(chan string, 128)
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 1<<20), 4<<20)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	return &mcpProcess{cmd: cmd, stdin: stdin, lines: lines, stderr: stderrFile}
}

// notify sends a JSON-RPC notification (no id).
func (p *mcpProcess) notify(line string) {
	_, _ = fmt.Fprintln(p.stdin, line)
}

// call sends a request and waits for a response with the same id.
func (p *mcpProcess) call(t *testing.T, id int, method, params string) string {
	t.Helper()
	req := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":%s}`, id, method, params)
	if _, err := fmt.Fprintln(p.stdin, req); err != nil {
		t.Fatalf("write to stdin: %v", err)
	}
	timeout := time.After(3 * time.Minute) // headroom for cold start of embeddings
	for {
		select {
		case line, ok := <-p.lines:
			if !ok {
				t.Fatalf("server closed stdout; stderr:\n%s", p.stderrText())
			}
			var resp struct {
				ID *int `json:"id"`
			}
			if err := json.Unmarshal([]byte(line), &resp); err != nil {
				continue
			}
			if resp.ID != nil && *resp.ID == id {
				return line
			}
		case <-timeout:
			t.Fatalf("response timeout id=%d; stderr:\n%s", id, p.stderrText())
		}
	}
}

// stderrText reads the accumulated stderr of the subprocess.
func (p *mcpProcess) stderrText() string {
	data, _ := os.ReadFile(p.stderr.Name())
	return string(data)
}

// stop closes stdin (EOF → graceful shutdown) with a fallback kill.
func (p *mcpProcess) stop(t *testing.T) {
	t.Helper()
	_ = p.stdin.Close()
	done := make(chan struct{})
	go func() {
		_ = p.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = p.cmd.Process.Kill()
		<-done
	}
}
