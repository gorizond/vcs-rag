package github

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorizond/vcs-rag/internal/config"
)

// TestListRepositoriesAllowlist: GITHUB_REPO owner/repo entries are unioned
// with the organization listing — duplicates collapse by ID and a repo that
// cannot be fetched (404) is a WARN, not a failure.
func TestListRepositoriesAllowlist(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch strings.TrimPrefix(r.URL.Path, "/api/v3") {
		case "/orgs/acme/repos":
			_, _ = w.Write([]byte(`[
				{"id":1,"name":"a","owner":{"login":"acme"},"clone_url":"https://gh/acme/a.git","default_branch":"main"},
				{"id":2,"name":"b","owner":{"login":"acme"},"clone_url":"https://gh/acme/b.git","default_branch":"main"}
			]`))
		case "/repos/acme/b":
			_, _ = w.Write([]byte(`{"id":2,"name":"b","owner":{"login":"acme"},"clone_url":"https://gh/acme/b.git","default_branch":"main"}`))
		case "/repos/acme/c":
			_, _ = w.Write([]byte(`{"id":3,"name":"c","owner":{"login":"acme"},"clone_url":"https://gh/acme/c.git","default_branch":"main"}`))
		case "/repos/acme/missing":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(old)

	cfg := &config.Config{}
	cfg.Provider.Type = "github"
	cfg.Provider.GitHubBaseURL = srv.URL
	cfg.Provider.GitHubOrgs = []string{"acme"}
	cfg.Provider.GitHubRepos = []string{"acme/b", "acme/c", "acme/missing"}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	repos, err := c.ListRepositories(context.Background())
	if err != nil {
		t.Fatalf("ListRepositories: %v", err)
	}
	if len(repos) != 3 || repos[0].ID != "1" || repos[1].ID != "2" || repos[2].ID != "3" {
		t.Fatalf("repos = %+v, want IDs 1,2,3 (union, deduplicated)", repos)
	}
	if repos[2].Namespace != "acme" || repos[2].Path != "c" {
		t.Errorf("allowlist repo = %+v, want acme/c", repos[2])
	}
	if !strings.Contains(buf.String(), "skipping repo") || !strings.Contains(buf.String(), "acme/missing") {
		t.Errorf("missing WARN about the skipped repo: %q", buf.String())
	}
}
