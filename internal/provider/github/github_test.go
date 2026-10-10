package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gorizond/vcs-rag/internal/config"
)

// T143: the token is optional — an empty token builds an anonymous client (no
// Authorization header at all); a non-empty one is sent as a Bearer token.
func TestClientOptionalAuth(t *testing.T) {
	var mu sync.Mutex
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	run := func(token string) string {
		t.Helper()
		cfg := &config.Config{}
		cfg.Provider.Type = "github"
		cfg.Provider.GitHubOrgs = []string{"acme"}
		cfg.Provider.GitHubToken = token
		cfg.Provider.GitHubBaseURL = srv.URL
		c, err := New(cfg)
		if err != nil {
			t.Fatalf("New(token=%q): %v", token, err)
		}
		mu.Lock()
		auth = ""
		mu.Unlock()
		if _, err := c.ListRepositories(context.Background()); err != nil {
			t.Fatalf("ListRepositories(token=%q): %v", token, err)
		}
		mu.Lock()
		defer mu.Unlock()
		return auth
	}

	if got := run(""); got != "" {
		t.Errorf("the anonymous request carried Authorization = %q", got)
	}
	if got := run("ghp-EXAMPLE-NOT-A-TOKEN"); !strings.Contains(got, "ghp-EXAMPLE-NOT-A-TOKEN") {
		t.Errorf("the token request Authorization = %q", got)
	}
}
