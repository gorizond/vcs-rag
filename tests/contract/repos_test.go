package contract

import (
	"context"
	"reflect"
	"testing"

	"github.com/gorizond/vcs-rag/internal/provider"
	"github.com/gorizond/vcs-rag/internal/provider/fake"
)

// TestMergeRepos: дубли по Repo.ID схлопываются в один элемент,
// порядок — по первому вхождению (стабильный).
func TestMergeRepos(t *testing.T) {
	in := []provider.Repo{
		{ID: "1", Path: "a"},
		{ID: "2", Path: "b"},
		{ID: "1", Path: "a-dup"},
		{ID: "3", Path: "c"},
		{ID: "2", Path: "b-dup"},
	}
	want := []provider.Repo{
		{ID: "1", Path: "a"},
		{ID: "2", Path: "b"},
		{ID: "3", Path: "c"},
	}
	if got := provider.MergeRepos(in); !reflect.DeepEqual(got, want) {
		t.Errorf("MergeRepos = %+v, want %+v", got, want)
	}
	if got := provider.MergeRepos(nil); len(got) != 0 {
		t.Errorf("MergeRepos(nil) = %+v, want пусто", got)
	}
}

// TestFakeListingInvariants: выдача листинга соблюдает контракт —
// непустые ID/Namespace/Path/CloneURL, ID уникальны.
func TestFakeListingInvariants(t *testing.T) {
	f := &fake.Provider{Repos: []provider.Repo{
		{ID: "1", Namespace: "grp", Path: "grp/a", CloneURL: "https://example.com/grp/a.git", DefaultBranch: "main"},
		{ID: "2", Namespace: "grp/sub", Path: "grp/sub/b", CloneURL: "https://example.com/grp/sub/b.git", DefaultBranch: "main"},
	}}
	repos, err := f.ListRepositories(context.Background())
	if err != nil {
		t.Fatalf("ListRepositories: %v", err)
	}
	seen := make(map[string]bool)
	for _, r := range repos {
		if r.ID == "" || r.Namespace == "" || r.Path == "" || r.CloneURL == "" {
			t.Errorf("repo %+v: пустые обязательные поля", r)
		}
		if seen[r.ID] {
			t.Errorf("repo ID %q не уникален", r.ID)
		}
		seen[r.ID] = true
	}
}
