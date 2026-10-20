package gitengine

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// Fixtures are explicit example markers, not real tokens (T121).
const (
	exampleGitLabToken = "glpat-EXAMPLE-NOT-A-TOKEN"
	exampleGitHubToken = "ghp-EXAMPLE-NOT-A-TOKEN"
)

// TestAuthEnv checks the Basic pair and header per provider type (T074).
func TestAuthEnv(t *testing.T) {
	gitlab := AuthEnv("gitlab", exampleGitLabToken)
	assertEnvKV(t, gitlab, "GIT_CONFIG_COUNT", "1")
	assertEnvKV(t, gitlab, "GIT_CONFIG_KEY_0", "http.extraHeader")
	assertEnvKV(t, gitlab, "GIT_CONFIG_VALUE_0",
		"Authorization: Basic "+base64.StdEncoding.EncodeToString([]byte("oauth2:"+exampleGitLabToken)))

	gh := AuthEnv("github", exampleGitHubToken)
	assertEnvKV(t, gh, "GIT_CONFIG_COUNT", "1")
	assertEnvKV(t, gh, "GIT_CONFIG_KEY_0", "http.extraHeader")
	assertEnvKV(t, gh, "GIT_CONFIG_VALUE_0",
		"Authorization: Basic "+base64.StdEncoding.EncodeToString([]byte("x-access-token:"+exampleGitHubToken)))

	if AuthEnv("gitlab", "") != nil {
		t.Error("empty token: env must be nil")
	}

	// T036/FR-016: the FIRST network command of a fresh mirror already carries
	// the full GIT_CONFIG_* authorization env — no anonymous first request
	// (failed-auth IP ban risk). The wrapper proves it without any network.
	t.Run("first network command is authenticated", func(t *testing.T) {
		src := initLocalRepo(t)
		logPath := filepath.Join(t.TempDir(), "git.log")
		eng := New(t.TempDir())
		eng.GitBin = gitWrapper(t, logPath) // the existing env/argv logger
		eng.AuthEnv = AuthEnv("gitlab", exampleGitLabToken)
		repo := provider.Repo{ID: "1", Namespace: "grp", Path: "fixture",
			CloneURL: "file://" + src, DefaultBranch: "main"}

		if _, err := eng.EnsureMirror(context.Background(), "gitlab", repo); err != nil {
			t.Fatalf("EnsureMirror (first network command): %v", err)
		}
		data, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatalf("wrapper log: %v", err)
		}
		first := findLine(string(data), "ARGS:clone")
		if first == "" {
			t.Fatalf("no clone in the wrapper log:\n%s", data)
		}
		if !strings.Contains(first, "CONFIG:1|http.extraHeader") {
			t.Errorf("first command lacks GIT_CONFIG_COUNT/http.extraHeader: %s", first)
		}
		want := "ENV:Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("oauth2:"+exampleGitLabToken))
		if !strings.Contains(first, want) {
			t.Errorf("first command lacks the Authorization header: %s", first)
		}
		if strings.Contains(first, exampleGitLabToken) {
			t.Errorf("token leaked into argv of the first command: %s", first)
		}
	})
}

func assertEnvKV(t *testing.T, env []string, key, want string) {
	t.Helper()
	for _, kv := range env {
		if strings.HasPrefix(kv, key+"=") {
			if got := strings.TrimPrefix(kv, key+"="); got != want {
				t.Errorf("%s = %q, want %q", key, got, want)
			}
			return
		}
	}
	t.Errorf("%s not found in env %v", key, env)
}

// gitWrapper installs a git wrapper script that logs the received env and argv.
func gitWrapper(t *testing.T, logPath string) string {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git not found: %v", err)
	}
	script := "#!/bin/sh\n" +
		"if [ -n \"$GIT_CONFIG_COUNT\" ]; then\n" +
		"  printf 'ENV:%s CONFIG:%s|%s ARGS:%s\\n' \"$GIT_CONFIG_VALUE_0\" \"$GIT_CONFIG_COUNT\" \"$GIT_CONFIG_KEY_0\" \"$*\" >> '" + logPath + "'\n" +
		"else\n" +
		"  printf 'ENV:%s ARGS:%s\\n' \"$GIT_CONFIG_VALUE_0\" \"$*\" >> '" + logPath + "'\n" +
		"fi\n" +
		"exec '" + realGit + "' \"$@\"\n"
	path := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// initLocalRepo creates a local source repository with a single commit.
func initLocalRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "init")
	return dir
}

// TestCloneAndFetchGetAuthEnv verifies that only network operations
// (clone/fetch) receive the auth env, the clone URL is clean, and no token
// ends up in the mirror (T074).
func TestCloneAndFetchGetAuthEnv(t *testing.T) {
	ctx := context.Background()
	src := initLocalRepo(t)
	logPath := filepath.Join(t.TempDir(), "git.log")
	eng := New(t.TempDir())
	eng.GitBin = gitWrapper(t, logPath)
	eng.AuthEnv = AuthEnv("gitlab", exampleGitLabToken)

	repo := provider.Repo{ID: "1", Namespace: "grp", Path: "fixture", CloneURL: "file://" + src, DefaultBranch: "main"}
	mirror, err := eng.EnsureMirror(ctx, "gitlab", repo)
	if err != nil {
		t.Fatalf("EnsureMirror (clone): %v", err)
	}
	if _, err := eng.EnsureMirror(ctx, "gitlab", repo); err != nil {
		t.Fatalf("EnsureMirror (fetch): %v", err)
	}
	if _, err := eng.RevList(ctx, mirror, "main", 0); err != nil {
		t.Fatalf("RevList: %v", err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("wrapper log: %v", err)
	}
	log := string(data)
	wantCred := base64.StdEncoding.EncodeToString([]byte("oauth2:" + exampleGitLabToken))

	for _, tc := range []struct{ name, args string }{
		{"clone", "ARGS:clone --mirror " + repo.CloneURL + " " + mirror},
		{"fetch", "ARGS:remote update --prune"},
	} {
		line := findLine(log, tc.args)
		if line == "" {
			t.Fatalf("%s: no log line %q\n%s", tc.name, tc.args, log)
		}
		if !strings.Contains(line, "ENV:Authorization: Basic "+wantCred) {
			t.Errorf("%s: missing auth env: %s", tc.name, line)
		}
		if strings.Contains(line, exampleGitLabToken) {
			t.Errorf("%s: token leaked into argv: %s", tc.name, line)
		}
	}

	// local operation — no auth env
	local := findLine(log, "ARGS:rev-list")
	if local == "" {
		t.Fatalf("no rev-list in the log\n%s", log)
	}
	if !strings.HasPrefix(local, "ENV: ARGS:") {
		t.Errorf("local operation received auth env: %s", local)
	}

	// negative control: neither the token nor the base64 pair is in the mirror
	for _, needle := range []string{exampleGitLabToken, wantCred} {
		out, err := exec.Command("grep", "-r", "-l", "--", needle, mirror).CombinedOutput()
		if err == nil && len(out) > 0 {
			t.Errorf("negative control: %q found in the mirror: %s", needle, out)
		}
	}
	// clone URL in the mirror config is clean
	urlOut, err := exec.Command(eng.GitBin, "-C", mirror, "config", "--get", "remote.origin.url").Output()
	if err != nil || strings.TrimSpace(string(urlOut)) != repo.CloneURL {
		t.Errorf("remote.origin.url = %q (err=%v), want clean URL %q",
			strings.TrimSpace(string(urlOut)), err, repo.CloneURL)
	}
}

// findLine returns the first log line containing substr.
func findLine(log, substr string) string {
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, substr) {
			return line
		}
	}
	return ""
}
