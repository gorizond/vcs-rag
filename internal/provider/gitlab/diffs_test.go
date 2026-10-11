package gitlab

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorizond/vcs-rag/internal/config"
	"github.com/gorizond/vcs-rag/internal/provider"
)

// TestListChangeRequestDiffsPaginationAndFlags — US2/T022: pages of MR diffs
// are followed, per_page stays at 100, the normalized status vocabulary is
// produced, and GitLab's too_large/collapsed flags mark the file truncated.
func TestListChangeRequestDiffsPaginationAndFlags(t *testing.T) {
	pages := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/projects/acme/demo/merge_requests/7/diffs") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("per_page"); got != "100" {
			t.Errorf("per_page = %q, want 100", got)
		}
		page := r.URL.Query().Get("page")
		pages = append(pages, page)
		w.Header().Set("Content-Type", "application/json")
		if page == "" {
			w.Header().Set("X-Next-Page", "2")
			_, _ = w.Write([]byte(`[{"old_path":"a.go","new_path":"a.go","diff":"@@ small @@","new_file":false,"renamed_file":false,"deleted_file":false}]`))
			return
		}
		_, _ = w.Write([]byte(`[
			{"old_path":"old.go","new_path":"new.go","diff":"@@ rename @@","renamed_file":true},
			{"old_path":"big.go","new_path":"big.go","diff":"","too_large":true}
		]`))
	}))
	defer srv.Close()

	cfg := &config.Config{}
	cfg.Provider.Type = "gitlab"
	cfg.Provider.GitLabToken = "test-token"
	cfg.Provider.GitLabBaseURL = srv.URL
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	files, err := c.ListChangeRequestDiffs(context.Background(), provider.Repo{Namespace: "acme", Path: "demo"}, 7)
	if err != nil {
		t.Fatalf("ListChangeRequestDiffs: %v", err)
	}
	if len(files) != 3 || len(pages) != 2 {
		t.Fatalf("files=%d pages=%v, want 3 files over 2 pages", len(files), pages)
	}
	if files[0].Status != "modified" || files[0].Truncated || files[0].Patch != "@@ small @@" {
		t.Errorf("modified file mapped: %+v", files[0])
	}
	if files[1].Status != "renamed" || files[1].OldPath != "old.go" || files[1].NewPath != "new.go" {
		t.Errorf("renamed file mapped: %+v", files[1])
	}
	if files[2].Status != "modified" || !files[2].Truncated || files[2].MRID != 7 {
		t.Errorf("too-large file mapped: %+v", files[2])
	}
}
