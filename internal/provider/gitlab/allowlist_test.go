package gitlab

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

// TestListRepositoriesAllowlist: GITLAB_PROJECT_ID entries are unioned with
// the group listing — a numeric ID and a full path both resolve through
// GET /projects/:id, duplicates collapse by ID, and an unfetchable project
// (404) is a WARN, not a failure.
func TestListRepositoriesAllowlist(t *testing.T) {
	var requested []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v4/groups/g1/projects":
			_, _ = w.Write([]byte(`[
				{"id":1,"path":"a","namespace":{"full_path":"g1"},"http_url_to_repo":"https://gl/g1/a.git","default_branch":"main"},
				{"id":2,"path":"b","namespace":{"full_path":"g1"},"http_url_to_repo":"https://gl/g1/b.git","default_branch":"main"}
			]`))
		case r.URL.EscapedPath() == "/api/v4/projects/2":
			requested = append(requested, "2")
			_, _ = w.Write([]byte(`{"id":2,"path":"b","namespace":{"full_path":"g1"},"http_url_to_repo":"https://gl/g1/b.git","default_branch":"main"}`))
		case r.URL.EscapedPath() == "/api/v4/projects/g1%2Fc":
			requested = append(requested, "g1/c")
			_, _ = w.Write([]byte(`{"id":3,"path":"c","namespace":{"full_path":"g1"},"http_url_to_repo":"https://gl/g1/c.git","default_branch":"main"}`))
		case r.URL.EscapedPath() == "/api/v4/projects/999":
			requested = append(requested, "999")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"404 Project Not Found"}`))
		default:
			t.Errorf("unexpected path %q", r.URL.EscapedPath())
		}
	}))
	defer srv.Close()

	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(old)

	cfg := &config.Config{}
	cfg.Provider.Type = "gitlab"
	cfg.Provider.GitLabToken = "test-token"
	cfg.Provider.GitLabBaseURL = srv.URL
	cfg.Provider.GitLabGroups = []string{"g1"}
	cfg.Provider.GitLabProjects = []string{"2", "g1/c", "999"}
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
	if repos[2].Namespace != "g1" || repos[2].Path != "c" {
		t.Errorf("allowlist repo = %+v, want g1/c", repos[2])
	}
	if len(requested) != 3 {
		t.Errorf("allowlist refs requested = %v, want all three", requested)
	}
	if !strings.Contains(buf.String(), "skipping project") || !strings.Contains(buf.String(), "999") {
		t.Errorf("missing WARN about the skipped project: %q", buf.String())
	}
}
