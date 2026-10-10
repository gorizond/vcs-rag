// Package integration — integration tests on local git fixtures.
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

// runGit runs git in the dir directory and returns stdout.
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

// writeFile writes a file, creating directories as needed.
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

// initSourceRepo creates a source repository:
// c1: main.go; c2: M main.go + A util.go; c3: D util.go; feature branch (c4).
// Returns the path and main shas in oldest→newest order (c1..c3).
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

// newFixtureMirror mirrors the source and returns the engine, mirror path and shas.
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

// TestMirrorSnapshotAndUpdate: clone --mirror, path scheme, branch snapshot,
// remote update after a new commit.
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
		t.Errorf("mirror path %q, want %q", mirror, wantPath)
	}

	snap, err := eng.Snapshot(ctx, mirror)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snap) != 2 {
		t.Errorf("branches %d (%v), expected 2 (main, feature)", len(snap), snap)
	}
	if snap["main"] != shas[2] {
		t.Errorf("tip main = %q, want %q", snap["main"], shas[2])
	}
	if snap["feature"] == "" {
		t.Error("feature branch missing")
	}

	// new commit in the source → remote update sees it
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
		t.Error("main tip was not updated after remote update")
	}
}

// TestDiffCommit: A/M/D statuses, --root for the first commit.
func TestDiffCommit(t *testing.T) {
	ctx := context.Background()
	eng, mirror, _, shas := newFixtureMirror(t)

	ch1, err := eng.DiffCommit(ctx, mirror, shas[0])
	if err != nil {
		t.Fatalf("DiffCommit c1: %v", err)
	}
	if !hasChange(ch1, "main.go", "A") {
		t.Errorf("c1 (--root): %v, expected A main.go", ch1)
	}
	ch2, err := eng.DiffCommit(ctx, mirror, shas[1])
	if err != nil {
		t.Fatalf("DiffCommit c2: %v", err)
	}
	if !hasChange(ch2, "main.go", "M") || !hasChange(ch2, "util.go", "A") {
		t.Errorf("c2: %v, expected M main.go + A util.go", ch2)
	}
	ch3, err := eng.DiffCommit(ctx, mirror, shas[2])
	if err != nil {
		t.Fatalf("DiffCommit c3: %v", err)
	}
	if !hasChange(ch3, "util.go", "D") {
		t.Errorf("c3: %v, expected D util.go", ch3)
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

// TestTreeFilesAndBlobReader: ls-tree sizes/filter, batch blob reading,
// missing object, binary detection.
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
		t.Fatalf("main.go not found: %v", files)
	}
	wantMain := "package main\n\nfunc main() { println(1) }\n"
	if mainFile.Size != int64(len(wantMain)) {
		t.Errorf("main.go size = %d, want %d", mainFile.Size, len(wantMain))
	}

	// the size filter cuts off everything before reading
	tiny, err := eng.TreeFiles(ctx, mirror, shas[1], 10)
	if err != nil {
		t.Fatalf("TreeFiles (filter): %v", err)
	}
	if len(tiny) != 0 {
		t.Errorf("size>10 filter: %d files, expected 0", len(tiny))
	}

	// batch reading: two objects in a row + a missing one
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
		t.Errorf("main.go content = %q, want %q", content, wantMain)
	}
	utilContent, found, err := reader.ReadBlob(files["util.go"].SHA)
	if err != nil || !found || !strings.Contains(string(utilContent), "func util") {
		t.Fatalf("ReadBlob(util.go): found=%v err=%v content=%q", found, err, utilContent)
	}
	if _, found, err := reader.ReadBlob("0000000000000000000000000000000000000000"); err != nil || found {
		t.Errorf("ReadBlob(missing): found=%v err=%v, expected false/nil", found, err)
	}

	// binary file: NUL in the first 8000 bytes
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
		t.Fatalf("bin.dat not found: %v", tree2)
	}
	binContent, found, err := reader.ReadBlob(binSHA)
	if err != nil || !found {
		t.Fatalf("ReadBlob(bin.dat): found=%v err=%v", found, err)
	}
	if !gitengine.IsBinary(binContent) {
		t.Error("IsBinary(bin.dat) = false, expected true")
	}
	if gitengine.IsBinary(content) {
		t.Error("IsBinary(main.go) = true, expected false")
	}
}

// TestForcePush: branch rewrite changes the snapshot, the old tip is unreachable from the new one.
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

	// rewrite main: reset to the first commit and new history
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
		t.Fatal("main tip did not change after the rewrite")
	}
	rev, err := eng.RevList(ctx, mirror, after["main"], 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, sha := range rev {
		if sha == before["main"] {
			t.Errorf("old tip %s is reachable from the new one — prune will not work", before["main"])
		}
	}
}
