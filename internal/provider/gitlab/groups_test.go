package gitlab

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// TestListGroups — US4/T038: groups map onto the normalized id/full_path/path/
// name/parent_id shape across pages.
func TestListGroups(t *testing.T) {
	pages := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/groups") {
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
			_, _ = w.Write([]byte(`[
				{"id":1,"name":"Acme","path":"acme","full_path":"acme","parent_id":null}]`))
			return
		}
		_, _ = w.Write([]byte(`[
			{"id":2,"name":"Sub","path":"sub","full_path":"acme/sub","parent_id":1}]`))
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)
	groups, err := c.ListGroups(context.Background())
	if err != nil {
		t.Fatalf("ListGroups: %v", err)
	}
	if len(groups) != 2 || len(pages) != 2 {
		t.Fatalf("groups=%d pages=%v, want 2 over 2 pages", len(groups), pages)
	}
	if groups[0].ID != "1" || groups[0].FullPath != "acme" || groups[0].Name != "Acme" || groups[0].ParentID != "" {
		t.Errorf("top-level group mapped: %+v", groups[0])
	}
	if groups[1].ID != "2" || groups[1].FullPath != "acme/sub" || groups[1].ParentID != "1" {
		t.Errorf("subgroup mapped: %+v", groups[1])
	}
}

// TestListGroupProjects — US4/T038: group projects are listed with
// include_subgroups, every page is followed, and namespace kind/creator map.
func TestListGroupProjects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/groups/acme/projects") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("include_subgroups"); got != "true" {
			t.Errorf("include_subgroups = %q, want true", got)
		}
		if got := r.URL.Query().Get("per_page"); got != "100" {
			t.Errorf("per_page = %q, want 100", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"id":10,"name":"Demo","path":"demo","path_with_namespace":"acme/demo",
			 "creator_id":7,"namespace":{"id":1,"kind":"group","full_path":"acme"}},
			{"id":11,"name":"Tool","path":"tool","path_with_namespace":"bob/tool","creator_id":0}]`))
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)
	projects, err := c.ListGroupProjects(context.Background(), "acme")
	if err != nil {
		t.Fatalf("ListGroupProjects: %v", err)
	}
	if len(projects) != 2 {
		t.Fatalf("projects=%d, want 2", len(projects))
	}
	if projects[0].ID != "10" || projects[0].FullPath != "acme/demo" || projects[0].Name != "Demo" ||
		projects[0].NamespaceKind != "group" || projects[0].CreatorID != "7" {
		t.Errorf("group project mapped: %+v", projects[0])
	}
	if projects[1].NamespaceKind != "" || projects[1].CreatorID != "" {
		t.Errorf("absent namespace/creator mapped: %+v", projects[1])
	}
}

// TestListGroupsUnavailable — US4/T038: a 403 is a capability signal, not a
// failure.
func TestListGroupsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"403 Forbidden"}`))
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)
	if _, err := c.ListGroups(context.Background()); !errors.Is(err, provider.ErrDomainUnavailable) {
		t.Fatalf("ListGroups err = %v, want ErrDomainUnavailable", err)
	}
}
