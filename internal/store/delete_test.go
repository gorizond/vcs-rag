package store

import "testing"

// T140: the GC repo-scope filter is the payload triple (vcs_type, group_org,
// repo); an empty vcs_type is omitted.
func TestRepoPointsFilter(t *testing.T) {
	flt := repoPointsFilter("gitlab", "example/group/gitops", "databases")
	if len(flt.GetMust()) != 3 {
		t.Fatalf("must = %d conditions, want 3", len(flt.GetMust()))
	}
	want := []struct{ key, value string }{
		{"vcs_type", "gitlab"},
		{"group_org", "example/group/gitops"},
		{"repo", "databases"},
	}
	for i, w := range want {
		c := flt.GetMust()[i]
		if k, v := fieldKeyword(c); k != w.key || v != w.value {
			t.Errorf("must[%d] = %s=%q, want %s=%q", i, k, v, w.key, w.value)
		}
	}

	empty := repoPointsFilter("", "grp", "r")
	if len(empty.GetMust()) != 2 {
		t.Fatalf("empty vcs_type: must = %d, want 2", len(empty.GetMust()))
	}
	if k, _ := fieldKeyword(empty.GetMust()[0]); k != "group_org" {
		t.Errorf("empty vcs_type first condition = %q, want group_org", k)
	}
}

// T140: DeleteByIDs of an empty list is a no-op (no client call).
func TestDeleteByIDsEmpty(t *testing.T) {
	s := &Store{}
	if err := s.DeleteByIDs(nil, nil); err != nil {
		t.Errorf("DeleteByIDs(nil) = %v, want nil", err)
	}
}
