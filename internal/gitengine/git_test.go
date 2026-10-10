package gitengine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// TestEnsureLocalMirrorNoNetwork — T083: with an existing mirror the index
// lane must not touch the remote. The remote is replaced with an unusable one:
// a fetch would fail, as the EnsureMirror negative control shows. The git
// wrapper log proves EnsureLocalMirror starts no git process at all.
func TestEnsureLocalMirrorNoNetwork(t *testing.T) {
	ctx := context.Background()
	src := initLocalRepo(t)
	logPath := filepath.Join(t.TempDir(), "git.log")
	eng := New(t.TempDir())
	eng.GitBin = gitWrapper(t, logPath)

	repo := provider.Repo{ID: "1", Namespace: "grp", Path: "fixture", CloneURL: "file://" + src, DefaultBranch: "main"}
	mirror, err := eng.EnsureMirror(ctx, "gitlab", repo) // the first clone is a network operation
	if err != nil {
		t.Fatalf("EnsureMirror (clone): %v", err)
	}

	// the remote becomes unusable: any clone/fetch would fail.
	bogus := "file://" + filepath.Join(t.TempDir(), "void.git")
	if out, err := exec.Command("git", "-C", mirror, "remote", "set-url", "origin", bogus).CombinedOutput(); err != nil {
		t.Fatalf("remote set-url: %v: %s", err, out)
	}

	// everything the wrapper logs after the marker belongs to EnsureLocalMirror.
	const marker = "MARKER local-mirror"
	if err := os.WriteFile(logPath, []byte(marker+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := eng.EnsureLocalMirror(ctx, "gitlab", repo)
	if err != nil {
		t.Fatalf("EnsureLocalMirror: %v", err)
	}
	if got != mirror {
		t.Errorf("path = %q, want the existing mirror %q", got, mirror)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if log := strings.TrimSpace(string(data)); log != marker {
		t.Errorf("EnsureLocalMirror invoked git: %s", log)
	}

	// negative control: the mirror lane (EnsureMirror) tries the remote and fails.
	if _, err := eng.EnsureMirror(ctx, "gitlab", repo); err == nil {
		t.Fatal("EnsureMirror with an unusable remote must fail (control)")
	}
}
