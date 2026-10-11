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

// TestListMergeRequestNodesMergeMetadata — US2/T024: the MR listing asks for
// all states, follows pages, and maps the merge metadata (merged,
// merge_commit_sha) plus the branch/author fields the merge_request node needs.
func TestListMergeRequestNodesMergeMetadata(t *testing.T) {
	pages := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/projects/acme/demo/merge_requests") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("state"); got != "all" {
			t.Errorf("state = %q, want all", got)
		}
		if got := r.URL.Query().Get("per_page"); got != "100" {
			t.Errorf("per_page = %q, want 100", got)
		}
		page := r.URL.Query().Get("page")
		pages = append(pages, page)
		w.Header().Set("Content-Type", "application/json")
		if page == "" {
			w.Header().Set("X-Next-Page", "2")
			_, _ = w.Write([]byte(`[{
				"iid":7,"title":"merged MR","state":"merged",
				"source_branch":"fix/x","target_branch":"main",
				"author":{"username":"alice"},"merge_commit_sha":"abc123",
				"web_url":"https://gitlab.example/acme/demo/-/merge_requests/7",
				"updated_at":"2026-10-01T10:00:00Z"}]`))
			return
		}
		_, _ = w.Write([]byte(`[{
			"iid":8,"title":"open MR","state":"opened",
			"source_branch":"feat/y","target_branch":"main",
			"author":{"username":"bob"},
			"web_url":"https://gitlab.example/acme/demo/-/merge_requests/8",
			"updated_at":"2026-10-02T10:00:00Z"}]`))
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)
	mrs, err := c.ListMergeRequestNodes(context.Background(), provider.Repo{Namespace: "acme", Path: "demo"})
	if err != nil {
		t.Fatalf("ListMergeRequestNodes: %v", err)
	}
	if len(mrs) != 2 || len(pages) != 2 {
		t.Fatalf("mrs=%d pages=%v, want 2 over 2 pages", len(mrs), pages)
	}
	if !mrs[0].Merged || mrs[0].MergeCommitSHA != "abc123" || mrs[0].Author != "alice" ||
		mrs[0].State != "merged" || mrs[0].SourceBranch != "fix/x" {
		t.Errorf("merged MR mapped: %+v", mrs[0])
	}
	if mrs[1].Merged || mrs[1].MergeCommitSHA != "" || mrs[1].State != "open" ||
		mrs[1].Author != "bob" || mrs[1].SourceBranch != "feat/y" {
		t.Errorf("open MR mapped: %+v", mrs[1])
	}
}

// TestListMergeRequestDiffRefs — US2/T024: every diff version maps onto the
// normalized triple with the version id as the version key.
func TestListMergeRequestDiffRefs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/projects/acme/demo/merge_requests/7/versions") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("per_page"); got != "100" {
			t.Errorf("per_page = %q, want 100", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"id":42,"base_commit_sha":"b1","start_commit_sha":"s1","head_commit_sha":"h1","state":"collected"},
			{"id":41,"base_commit_sha":"b0","start_commit_sha":"s0","head_commit_sha":"h0","state":"empty"}
		]`))
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)
	refs, err := c.ListMergeRequestDiffRefs(context.Background(), provider.Repo{Namespace: "acme", Path: "demo"}, 7)
	if err != nil {
		t.Fatalf("ListMergeRequestDiffRefs: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("refs=%d, want 2", len(refs))
	}
	if refs[0].Version != "42" || refs[0].BaseSHA != "b1" || refs[0].StartSHA != "s1" ||
		refs[0].HeadSHA != "h1" || refs[0].State != "collected" {
		t.Errorf("version 42 mapped: %+v", refs[0])
	}
	if refs[1].Version != "41" || refs[1].State != "empty" {
		t.Errorf("version 41 mapped: %+v", refs[1])
	}
}

// testClient builds a Client against a test server (token is a dummy: the
// server never validates it).
func testClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	cfg := &config.Config{}
	cfg.Provider.Type = "gitlab"
	cfg.Provider.GitLabToken = "test-token"
	cfg.Provider.GitLabBaseURL = baseURL
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}
