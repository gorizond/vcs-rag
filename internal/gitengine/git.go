// Package gitengine — обёртка exec git для зеркал репозиториев (FR-005/006).
// Рабочего дерева нет: объекты читаются из bare-зеркал потоково, в память.
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

// Engine выполняет git-команды над зеркалами в MirrorsDir.
type Engine struct {
	MirrorsDir string
	GitBin     string
}

// New создаёт движок; GitBin по умолчанию "git".
func New(mirrorsDir string) *Engine {
	return &Engine{MirrorsDir: mirrorsDir, GitBin: "git"}
}

// MirrorPath возвращает путь зеркала:
// <MirrorsDir>/<vcsType>/<namespace с "/"→"_">/<repo.Path>.git.
func (e *Engine) MirrorPath(vcsType string, repo provider.Repo) string {
	ns := strings.ReplaceAll(repo.Namespace, "/", "_")
	return filepath.Join(e.MirrorsDir, vcsType, ns, repo.Path+".git")
}

// StatePath — sidecar-файл состояния рядом с зеркалом.
func StatePath(mirrorPath string) string { return mirrorPath + ".state.json" }

// run выполняет git-команду в каталоге dir и возвращает stdout.
// GIT_TERMINAL_PROMPT=0 — никаких интерактивных запросов креденшелов.
func (e *Engine) run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, e.GitBin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// EnsureMirror клонирует зеркало при отсутствии, иначе обновляет
// (remote update --prune). Возвращает путь зеркала.
func (e *Engine) EnsureMirror(ctx context.Context, vcsType string, repo provider.Repo) (string, error) {
	path := e.MirrorPath(vcsType, repo)
	if _, err := os.Stat(filepath.Join(path, "HEAD")); err == nil {
		if _, err := e.run(ctx, path, "remote", "update", "--prune"); err != nil {
			return "", err
		}
		return path, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if _, err := e.run(ctx, filepath.Dir(path), "clone", "--mirror", repo.CloneURL, path); err != nil {
		return "", err
	}
	return path, nil
}

// Snapshot возвращает атомарный снимок веток (один вызов for-each-ref):
// короткое имя ветки → SHA. Формат: %(refname:short)%00%(objectname).
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

// RevList возвращает коммиты ref от нового к старому; limit>0 — не более limit.
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
