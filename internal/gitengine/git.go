// Package gitengine wraps exec git for repository mirrors (FR-005/006).
// There is no working tree: objects are streamed from bare mirrors into memory.
package gitengine

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// Engine runs git commands over mirrors in MirrorsDir.
type Engine struct {
	MirrorsDir string
	GitBin     string
	// AuthEnv is the environment for network operations (clone/fetch): a Basic
	// header for private repos. Local operations run without it (T074).
	AuthEnv []string
}

// New creates the engine; GitBin defaults to "git".
func New(mirrorsDir string) *Engine {
	return &Engine{MirrorsDir: mirrorsDir, GitBin: "git"}
}

// MirrorPath returns the mirror path:
// <MirrorsDir>/<vcsType>/<namespace with "/"→"_">/<repo.Path>.git.
func (e *Engine) MirrorPath(vcsType string, repo provider.Repo) string {
	ns := strings.ReplaceAll(repo.Namespace, "/", "_")
	return filepath.Join(e.MirrorsDir, vcsType, ns, repo.Path+".git")
}

// StatePath is the state sidecar file next to the mirror.
func StatePath(mirrorPath string) string { return mirrorPath + ".state.json" }

// run executes a local git command in dir and returns stdout.
// GIT_TERMINAL_PROMPT=0 — no interactive credential prompts.
func (e *Engine) run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return e.runWithEnv(ctx, dir, nil, args...)
}

// runNet executes a network git command (clone/fetch): AuthEnv is added to the
// environment when set (private repositories, T074).
func (e *Engine) runNet(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return e.runWithEnv(ctx, dir, e.AuthEnv, args...)
}

// runWithEnv runs git with the base and extra environment.
func (e *Engine) runWithEnv(ctx context.Context, dir string, extraEnv []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, e.GitBin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.Env = append(cmd.Env, extraEnv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// EnsureMirror clones the mirror when absent, otherwise updates it
// (remote update --prune). Both operations are network ones: they run with
// AuthEnv (private repos), while the clone URL stays clean (no credentials).
func (e *Engine) EnsureMirror(ctx context.Context, vcsType string, repo provider.Repo) (string, error) {
	path := e.MirrorPath(vcsType, repo)
	if _, err := os.Stat(filepath.Join(path, "HEAD")); err == nil {
		if _, err := e.runNet(ctx, path, "remote", "update", "--prune"); err != nil {
			return "", err
		}
		return path, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if _, err := e.runNet(ctx, filepath.Dir(path), "clone", "--mirror", repo.CloneURL, path); err != nil {
		return "", err
	}
	return path, nil
}

// Snapshot returns an atomic branch snapshot (single for-each-ref call):
// short branch name → SHA. Format: %(refname:short)%00%(objectname).
func (e *Engine) Snapshot(ctx context.Context, mirror string) (map[string]string, error) {
	out, err := e.run(ctx, mirror, "for-each-ref", "refs/heads", "--format=%(refname:short)%00%(objectname)")
	if err != nil {
		return nil, err
	}
	snap := make(map[string]string)
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\x00", 2)
		if len(parts) != 2 {
			continue
		}
		snap[parts[0]] = parts[1]
	}
	return snap, nil
}

// RevList returns commits of ref from newest to oldest; limit>0 — at most limit.
func (e *Engine) RevList(ctx context.Context, mirror, ref string, limit int) ([]string, error) {
	args := []string{"rev-list"}
	if limit > 0 {
		args = append(args, fmt.Sprintf("--max-count=%d", limit))
	}
	args = append(args, ref)
	out, err := e.run(ctx, mirror, args...)
	if err != nil {
		return nil, err
	}
	var shas []string
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line != "" {
			shas = append(shas, line)
		}
	}
	return shas, nil
}
