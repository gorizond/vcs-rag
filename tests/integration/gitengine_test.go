// Package integration — интеграционные тесты на локальных git-fixture.
package integration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gorizond/vcs-rag/internal/gitengine"
	"github.com/gorizond/vcs-rag/internal/provider"
)

// runGit выполняет git в каталоге dir и возвращает stdout.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// writeFile пишет файл с созданием каталогов.
func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// initSourceRepo создаёт репозиторий-источник:
// c1: main.go; c2: M main.go + A util.go; c3: D util.go; ветка feature (c4).
// Возвращает путь и shas main в порядке oldest→newest (c1..c3).
func initSourceRepo(t *testing.T) (string, []string) {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-b", "main")
	writeFile(t, dir, "main.go", "package main\n\nfunc main() {}\n")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "c1: main.go")
	writeFile(t, dir, "main.go", "package main\n\nfunc main() { println(1) }\n")
	writeFile(t, dir, "util.go", "package main\n\nfunc util() {}\n")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "c2: modify main, add util")
	runGit(t, dir, "rm", "util.go")
	runGit(t, dir, "commit", "-m", "c3: delete util")
	runGit(t, dir, "checkout", "-b", "feature")
	writeFile(t, dir, "feature.txt", "feature\n")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "c4: feature branch")
	runGit(t, dir, "checkout", "main")
	return dir, strings.Fields(runGit(t, dir, "rev-list", "--reverse", "main"))
}

// newFixtureMirror зеркалит источник и возвращает движок, путь зеркала и shas.
func newFixtureMirror(t *testing.T) (*gitengine.Engine, string, provider.Repo, []string) {
	t.Helper()
	src, shas := initSourceRepo(t)
	eng := gitengine.New(t.TempDir())
	repo := provider.Repo{
		ID: "1", Namespace: "grp/sub", Path: "fixture",
		CloneURL: "file://" + src, DefaultBranch: "main",
	}
	mirror, err := eng.EnsureMirror(context.Background(), "gitlab", repo)
	if err != nil {
		t.Fatalf("EnsureMirror: %v", err)
	}
	return eng, mirror, repo, shas
}

// TestMirrorSnapshotAndUpdate: clone --mirror, схема пути, снимок веток,
// remote update после нового коммита.
func TestMirrorSnapshotAndUpdate(t *testing.T) {
	ctx := context.Background()
	src, shas := initSourceRepo(t)
	eng := gitengine.New(t.TempDir())
	repo := provider.Repo{ID: "1", Namespace: "grp/sub", Path: "fixture", CloneURL: "file://" + src, DefaultBranch: "main"}

	mirror, err := eng.EnsureMirror(ctx, "gitlab", repo)
	if err != nil {
		t.Fatalf("EnsureMirror: %v", err)
	}
	wantPath := filepath.Join(eng.MirrorsDir, "gitlab", "grp_sub", "fixture.git")
	if mirror != wantPath {
		t.Errorf("путь зеркала %q, want %q", mirror, wantPath)
	}

	snap, err := eng.Snapshot(ctx, mirror)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snap) != 2 {
		t.Errorf("веток %d (%v), ожидалось 2 (main, feature)", len(snap), snap)
	}
	if snap["main"] != shas[2] {
		t.Errorf("tip main = %q, want %q", snap["main"], shas[2])
	}
	if snap["feature"] == "" {
		t.Error("нет ветки feature")
	}

	// новый коммит в источнике → remote update видит его
	writeFile(t, src, "main.go", "package main\n\nfunc main() { println(2) }\n")
	runGit(t, src, "add", ".")
	runGit(t, src, "commit", "-m", "c5")
	if _, err := eng.EnsureMirror(ctx, "gitlab", repo); err != nil {
		t.Fatalf("EnsureMirror (update): %v", err)
	}
	snap2, err := eng.Snapshot(ctx, mirror)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap2["main"] == snap["main"] {
		t.Error("tip main не обновился после remote update")
	}
}

// TestDiffCommit: статусы A/M/D, --root для первого коммита.
func TestDiffCommit(t *testing.T) {
	ctx := context.Background()
	eng, mirror, _, shas := newFixtureMirror(t)

	ch1, err := eng.DiffCommit(ctx, mirror, shas[0])
	if err != nil {
		t.Fatalf("DiffCommit c1: %v", err)
	}
	if !hasChange(ch1, "main.go", "A") {
		t.Errorf("c1 (--root): %v, ожидался A main.go", ch1)
	}
	ch2, err := eng.DiffCommit(ctx, mirror, shas[1])
	if err != nil {
		t.Fatalf("DiffCommit c2: %v", err)
	}
	if !hasChange(ch2, "main.go", "M") || !hasChange(ch2, "util.go", "A") {
		t.Errorf("c2: %v, ожидались M main.go + A util.go", ch2)
	}
	ch3, err := eng.DiffCommit(ctx, mirror, shas[2])
	if err != nil {
		t.Fatalf("DiffCommit c3: %v", err)
	}
	if !hasChange(ch3, "util.go", "D") {
		t.Errorf("c3: %v, ожидался D util.go", ch3)
	}
}

func hasChange(changes []gitengine.FileChange, path, status string) bool {
	for _, ch := range changes {
		if ch.Path == path && ch.Status == status {
			return true
		}
	}
	return false
}

// TestTreeFilesAndBlobReader: ls-tree размеры/фильтр, batch-чтение blob,
// отсутствующий объект, бинарная детекция.
func TestTreeFilesAndBlobReader(t *testing.T) {
	ctx := context.Background()
	eng, mirror, repo, shas := newFixtureMirror(t)

	tree, err := eng.TreeFiles(ctx, mirror, shas[1], 1<<20)
	if err != nil {
		t.Fatalf("TreeFiles: %v", err)
	}
	files := map[string]gitengine.TreeFile{}
	for _, f := range tree {
		files[f.Path] = f
	}
	mainFile, ok := files["main.go"]
	if !ok {
		t.Fatalf("main.go не найден: %v", files)
	}
	wantMain := "package main\n\nfunc main() { println(1) }\n"
	if mainFile.Size != int64(len(wantMain)) {
		t.Errorf("размер main.go = %d, want %d", mainFile.Size, len(wantMain))
	}

	// фильтр по размеру отсекает всё до чтения
	tiny, err := eng.TreeFiles(ctx, mirror, shas[1], 10)
	if err != nil {
		t.Fatalf("TreeFiles (фильтр): %v", err)
	}
	if len(tiny) != 0 {
		t.Errorf("фильтр size>10: файлов %d, ожидалось 0", len(tiny))
	}

	// batch-чтение: два объекта подряд + отсутствующий
	reader, err := eng.NewBlobReader(ctx, mirror)
	if err != nil {
		t.Fatalf("NewBlobReader: %v", err)
	}
	defer reader.Close()

	content, found, err := reader.ReadBlob(mainFile.SHA)
	if err != nil || !found {
		t.Fatalf("ReadBlob(main.go): found=%v err=%v", found, err)
	}
	if string(content) != wantMain {
		t.Errorf("содержимое main.go = %q, want %q", content, wantMain)
	}
	utilContent, found, err := reader.ReadBlob(files["util.go"].SHA)
	if err != nil || !found || !strings.Contains(string(utilContent), "func util") {
		t.Fatalf("ReadBlob(util.go): found=%v err=%v content=%q", found, err, utilContent)
	}
	if _, found, err := reader.ReadBlob("0000000000000000000000000000000000000000"); err != nil || found {
		t.Errorf("ReadBlob(missing): found=%v err=%v, ожидалось false/nil", found, err)
	}

	// бинарный файл: NUL в первых 8000 байт
	writeFile(t, repo.CloneURL[len("file://"):], "bin.dat", "bin\x00data")
	runGit(t, repo.CloneURL[len("file://"):], "add", ".")
	runGit(t, repo.CloneURL[len("file://"):], "commit", "-m", "c5: binary")
	if _, err := eng.EnsureMirror(ctx, "gitlab", repo); err != nil {
		t.Fatalf("EnsureMirror: %v", err)
	}
	snap, err := eng.Snapshot(ctx, mirror)
	if err != nil {
		t.Fatal(err)
	}
	tree2, err := eng.TreeFiles(ctx, mirror, snap["main"], 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var binSHA string
	for _, f := range tree2 {
		if f.Path == "bin.dat" {
			binSHA = f.SHA
		}
	}
	if binSHA == "" {
		t.Fatalf("bin.dat не найден: %v", tree2)
	}
	binContent, found, err := reader.ReadBlob(binSHA)
	if err != nil || !found {
		t.Fatalf("ReadBlob(bin.dat): found=%v err=%v", found, err)
	}
	if !gitengine.IsBinary(binContent) {
		t.Error("IsBinary(bin.dat) = false, ожидалось true")
	}
	if gitengine.IsBinary(content) {
		t.Error("IsBinary(main.go) = true, ожидалось false")
	}
}

// TestForcePush: перезапись ветки меняет снапшот, старый tip недостижим из нового.
func TestForcePush(t *testing.T) {
	ctx := context.Background()
	src, shas := initSourceRepo(t)
	eng := gitengine.New(t.TempDir())
	repo := provider.Repo{ID: "1", Namespace: "grp", Path: "fixture", CloneURL: "file://" + src, DefaultBranch: "main"}

	mirror, err := eng.EnsureMirror(ctx, "gitlab", repo)
	if err != nil {
		t.Fatalf("EnsureMirror: %v", err)
	}
	before, err := eng.Snapshot(ctx, mirror)
	if err != nil {
		t.Fatal(err)
	}

	// rewrite main: к первому коммиту и новая история
	runGit(t, src, "reset", "--hard", shas[0])
	writeFile(t, src, "rewritten.txt", "new history\n")
	runGit(t, src, "add", ".")
	runGit(t, src, "commit", "-m", "c-rewrite")
	if _, err := eng.EnsureMirror(ctx, "gitlab", repo); err != nil {
		t.Fatalf("EnsureMirror (force): %v", err)
	}
	after, err := eng.Snapshot(ctx, mirror)
	if err != nil {
		t.Fatal(err)
	}
	if after["main"] == before["main"] {
		t.Fatal("tip main не изменился после перезаписи")
	}
	rev, err := eng.RevList(ctx, mirror, after["main"], 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, sha := range rev {
		if sha == before["main"] {
			t.Errorf("старый tip %s достижим из нового — prune не сработает", before["main"])
		}
	}
}
