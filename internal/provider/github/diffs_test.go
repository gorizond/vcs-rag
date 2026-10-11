package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorizond/vcs-rag/internal/config"
	"github.com/gorizond/vcs-rag/internal/provider"
)

// TestListChangeRequestDiffsPaginationAndMapping — US2/T023: pages of PR files
// are followed, per_page stays at the endpoint cap (100), the GitLab-style
// status vocabulary is produced, a rename keeps the previous path, and an
// omitted patch (too large/binary) is marked truncated.
func TestListChangeRequestDiffsPaginationAndMapping(t *testing.T) {
	pages := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/repos/acme/demo/pulls/7/files") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("per_page"); got != "100" {
			t.Errorf("per_page = %q, want 100", got)
		}
		page := r.URL.Query().Get("page")
		pages = append(pages, page)
		w.Header().Set("Content-Type", "application/json")
		if page == "" {
			w.Header().Set("Link", `<http://`+r.Host+`/api/v3/repos/acme/demo/pulls/7/files?page=2>; rel="next"`)
			_, _ = w.Write([]byte(`[{"filename":"legacy/a.go","status":"removed","patch":"@@ -1 +0 @@"}]`))
			return
		}
		_, _ = w.Write([]byte(`[
			{"filename":"b/renamed.go","status":"renamed","previous_filename":"a/old.go","patch":"@@ rename @@"},
			{"filename":"big/blob.bin","status":"modified"}
		]`))
	}))
	defer srv.Close()

	cfg := &config.Config{}
	cfg.Provider.Type = "github"
	cfg.Provider.GitHubOrgs = []string{"acme"}
	cfg.Provider.GitHubToken = "test-token"
	cfg.Provider.GitHubBaseURL = srv.URL
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
	if files[0].Status != "deleted" || files[0].OldPath != "legacy/a.go" || files[0].Patch != "@@ -1 +0 @@" || files[0].Truncated {
		t.Errorf("removed file mapped: %+v", files[0])
	}
	if files[1].Status != "renamed" || files[1].OldPath != "a/old.go" || files[1].NewPath != "b/renamed.go" {
		t.Errorf("renamed file mapped: %+v", files[1])
	}
	if files[2].Status != "modified" || !files[2].Truncated || files[2].Patch != "" {
		t.Errorf("patch-less file mapped: %+v", files[2])
	}
	if files[2].MRID != 7 {
		t.Errorf("MRID = %d, want 7", files[2].MRID)
	}
}
