package gitlab

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// TestListTreeRecursivePages — US3/T032: the tree is listed recursively, every
// page is followed, and entries map onto the normalized blob/tree shape.
func TestListTreeRecursivePages(t *testing.T) {
	pages := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/projects/acme/demo/repository/tree") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("recursive"); got != "true" {
			t.Errorf("recursive = %q, want true", got)
		}
		if got := r.URL.Query().Get("per_page"); got != "100" {
			t.Errorf("per_page = %q, want 100", got)
		}
		page := r.URL.Query().Get("page")
		pages = append(pages, page)
		w.Header().Set("Content-Type", "application/json")
		if page == "" {
			w.Header().Set("X-Next-Page", "2")
			_, _ = w.Write([]byte(`[
				{"id":"a1","name":"cmd","type":"tree","path":"cmd","mode":"040000"},
				{"id":"a2","name":"main.go","type":"blob","path":"cmd/main.go","mode":"100644"}]`))
			return
		}
		_, _ = w.Write([]byte(`[
			{"id":"b1","name":"README.md","type":"blob","path":"README.md","mode":"100644"}]`))
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)
	entries, err := c.ListTree(context.Background(), provider.Repo{Namespace: "acme", Path: "demo"})
	if err != nil {
		t.Fatalf("ListTree: %v", err)
	}
	if len(entries) != 3 || len(pages) != 2 {
		t.Fatalf("entries=%d pages=%v, want 3 over 2 pages", len(entries), pages)
	}
	if entries[0].Path != "cmd" || entries[0].Name != "cmd" || entries[0].Type != "tree" {
		t.Errorf("tree entry mapped: %+v", entries[0])
	}
	if entries[1].Path != "cmd/main.go" || entries[1].Type != "blob" {
		t.Errorf("blob entry mapped: %+v", entries[1])
	}
	if entries[2].Path != "README.md" || entries[2].Name != "README.md" || entries[2].Type != "blob" {
		t.Errorf("page-2 entry mapped: %+v", entries[2])
	}
}

// TestListBranches — US3/T032: branches map onto the normalized name/default/
// protected fields across pages.
func TestListBranches(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/projects/acme/demo/repository/branches") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("per_page"); got != "100" {
			t.Errorf("per_page = %q, want 100", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"name":"main","default":true,"protected":true},
			{"name":"feat/x","default":false,"protected":false}]`))
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)
	branches, err := c.ListBranches(context.Background(), provider.Repo{Namespace: "acme", Path: "demo"})
	if err != nil {
		t.Fatalf("ListBranches: %v", err)
	}
	if len(branches) != 2 {
		t.Fatalf("branches=%d, want 2", len(branches))
	}
	if branches[0].Name != "main" || !branches[0].Default || !branches[0].Protected {
		t.Errorf("main branch mapped: %+v", branches[0])
	}
	if branches[1].Name != "feat/x" || branches[1].Default || branches[1].Protected {
		t.Errorf("feature branch mapped: %+v", branches[1])
	}
}
