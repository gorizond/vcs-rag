package store

import (
	"testing"

	"github.com/qdrant/go-client/qdrant"
)

// fieldKeyword extracts the match keyword of a field condition ("" otherwise).
func fieldKeyword(c *qdrant.Condition) (string, string) {
	if f := c.GetField(); f != nil {
		return f.GetKey(), f.GetMatch().GetKeyword()
	}
	return "", ""
}

// T133-fix: the construction of the payload filter — a single repository is a
// direct (group_org, repo) Must pair; 2+ pairs become nested OR conditions.
func TestBuildFilterRepos(t *testing.T) {
	if flt := buildFilter(Filter{}); flt != nil {
		t.Errorf("empty filter = %+v, want nil", flt)
	}

	// passthrough: the raw short form as a Must condition, group_org kept.
	flt := buildFilter(Filter{GroupOrg: "grp", Repo: "databases", Type: "code"})
	if flt == nil || len(flt.GetShould()) != 0 || flt.GetMinShould() != nil {
		t.Fatalf("single repo filter = %+v", flt)
	}
	if len(flt.GetMust()) != 3 { // group_org + repo + type
		t.Fatalf("must = %d conditions, want 3", len(flt.GetMust()))
	}
	if k, v := fieldKeyword(flt.GetMust()[0]); k != "group_org" || v != "grp" {
		t.Errorf("group_org must = %s=%q", k, v)
	}
	if k, v := fieldKeyword(flt.GetMust()[1]); k != "repo" || v != "databases" {
		t.Errorf("repo must = %s=%q", k, v)
	}

	// a single pair is the same direct Must pair (group_org comes from the pair).
	one := buildFilter(Filter{RepoRefs: []RepoRef{{GroupOrg: "example/group/gitops", Repo: "databases"}}})
	if one == nil || len(one.GetMust()) != 2 {
		t.Fatalf("single pair must = %+v", one)
	}
	if k, v := fieldKeyword(one.GetMust()[0]); k != "group_org" || v != "example/group/gitops" {
		t.Errorf("pair group_org = %s=%q", k, v)
	}
	if k, v := fieldKeyword(one.GetMust()[1]); k != "repo" || v != "databases" {
		t.Errorf("pair repo = %s=%q", k, v)
	}

	// several pairs: nested (group_org AND repo) conditions in Should + min_should=1;
	// the pairs win over a loose GroupOrg/Repo.
	flt = buildFilter(Filter{
		GroupOrg: "top-group", Repo: "dbs",
		RepoRefs: []RepoRef{
			{GroupOrg: "grp/a", Repo: "databases"},
			{GroupOrg: "grp/b", Repo: "databases"},
			{GroupOrg: "", Repo: "standalone"},
		},
		Type: "code",
	})
	if len(flt.GetShould()) != 3 {
		t.Fatalf("should = %d conditions, want 3", len(flt.GetShould()))
	}
	for _, c := range flt.GetMust() {
		if k, _ := fieldKeyword(c); k == "repo" || k == "group_org" {
			t.Errorf("loose %s leaked into must: %+v", k, flt.GetMust())
		}
	}
	if len(flt.GetMust()) != 1 || flt.GetMust()[0].GetField().GetKey() != "type" {
		t.Errorf("other must conditions lost: %+v", flt.GetMust())
	}
	if ms := flt.GetMinShould(); ms == nil || ms.GetMinCount() != 1 || len(ms.GetConditions()) != 3 {
		t.Errorf("min_should = %+v", flt.GetMinShould())
	}
	// every Should condition is a nested filter carrying group_org (when set) + repo.
	for i, c := range flt.GetShould() {
		inner := c.GetFilter()
		if inner == nil {
			t.Fatalf("should[%d] is not a nested filter: %+v", i, c)
		}
		var keys []string
		for _, ic := range inner.GetMust() {
			k, _ := fieldKeyword(ic)
			keys = append(keys, k)
		}
		want := []string{"group_org", "repo"}
		if i == 2 {
			want = []string{"repo"} // an empty group_org is omitted
		}
		if len(keys) != len(want) || keys[0] != want[0] || keys[len(keys)-1] != want[len(want)-1] {
			t.Errorf("should[%d] nested must keys = %v, want %v", i, keys, want)
		}
	}
	if k, v := fieldKeyword(flt.GetShould()[0].GetFilter().GetMust()[1]); k != "repo" || v != "databases" {
		t.Errorf("should[0] repo = %s=%q", k, v)
	}
}

// T032: F3 filters — state/author/milestone are Match conditions; array fields
// use Match for one value and Match-any for several (ANY semantics); empty
// lists add no condition.
func TestBuildFilterMetadata(t *testing.T) {
	flt := buildFilter(Filter{
		Type: "issue", State: "open", Author: "alice", Milestone: "v1",
		Labels: []string{"bug"}, Assignees: []string{"bob"}, Reviewers: []string{"carol", "dave"},
	})
	if flt == nil || flt.GetMinShould() != nil {
		t.Fatalf("filter = %+v", flt)
	}
	got := map[string]string{}
	for _, c := range flt.GetMust() {
		k, v := fieldKeyword(c)
		got[k] = v
	}
	for k, want := range map[string]string{
		"type": "issue", "state": "open", "author": "alice", "milestone": "v1",
		"labels": "bug", "assignees": "bob",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
	// several reviewers: match-any, not a keyword match.
	var reviewers *qdrant.Condition
	for _, c := range flt.GetMust() {
		if c.GetField().GetKey() == "reviewers" {
			reviewers = c
		}
	}
	if reviewers == nil {
		t.Fatal("no reviewers condition")
	}
	keywords := reviewers.GetField().GetMatch().GetKeywords()
	if keywords == nil || len(keywords.GetStrings()) != 2 {
		t.Errorf("reviewers match = %+v, want match-any of 2", reviewers.GetField().GetMatch())
	}

	// empty arrays and empty strings add nothing.
	empty := buildFilter(Filter{Type: "issue"})
	if len(empty.GetMust()) != 1 || empty.GetMust()[0].GetField().GetKey() != "type" {
		t.Errorf("empty metadata leaked into must: %+v", empty.GetMust())
	}
}
