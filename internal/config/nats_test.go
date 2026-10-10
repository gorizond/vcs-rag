package config

import (
	"strings"
	"testing"
)

// T117: the builder scopes the consumer/subject/KV/connection by instance id
// and keeps the stream subject list shared.
func TestRefreshNames(t *testing.T) {
	a := RefreshNames("a")
	if a.Stream != NATSStream || len(a.Subjects) != 1 || a.Subjects[0] != NATSSubjects {
		t.Errorf("shared part = %q %v, want %q %q", a.Stream, a.Subjects, NATSStream, NATSSubjects)
	}
	if a.Subject != "vcsrag.a.refresh" || a.Consumer != "worker-refresh-a" ||
		a.KVBucket != "refresh_status_a" || a.ConnName != "vcs-rag-a" {
		t.Errorf("instance a = %+v", a)
	}
	// T150: the worker indexing status bucket is scoped by the same instance id.
	if a.WorkerKVBucket != "worker_status_a" {
		t.Errorf("WorkerKVBucket = %q, want worker_status_a", a.WorkerKVBucket)
	}

	d := RefreshNames("")
	if d.InstanceID != NATSDefaultInstance || d.Subject != "vcsrag.default.refresh" ||
		d.Consumer != "worker-refresh-default" || d.KVBucket != "refresh_status_default" ||
		d.WorkerKVBucket != "worker_status_default" {
		t.Errorf("default instance = %+v", d)
	}

	// the instances must not overlap: exact filters, different names.
	b := RefreshNames("b")
	if a.Subject == b.Subject || a.Consumer == b.Consumer || a.KVBucket == b.KVBucket ||
		a.WorkerKVBucket == b.WorkerKVBucket {
		t.Errorf("instances a/b overlap: %+v vs %+v", a, b)
	}
	for _, n := range []RefreshTopology{a, b, d} {
		if err := n.Validate(); err != nil {
			t.Errorf("Validate(%+v) = %v", n, err)
		}
	}
}

// T117: Validate rejects malformed ids and overlapping/wildcard filters.
func TestRefreshNamesValidateRejects(t *testing.T) {
	tests := []struct {
		name string
		mut  func(n *RefreshTopology)
		want string
	}{
		{"bad id", func(n *RefreshTopology) { n.InstanceID = "Bad Id" }, "instance id"},
		{"wildcard filter", func(n *RefreshTopology) { n.Subject = NATSSubjects }, "exact instance subject"},
		{"greater filter", func(n *RefreshTopology) { n.Subject = "vcsrag.>" }, "exact instance subject"},
		{"foreign subject", func(n *RefreshTopology) { n.Subject = "vcsrag.b.refresh" }, "exact instance subject"},
		{"foreign consumer", func(n *RefreshTopology) { n.Consumer = "worker-refresh-b" }, "consumer"},
		{"foreign bucket", func(n *RefreshTopology) { n.KVBucket = "refresh_status_b" }, "KV bucket"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n := RefreshNames("a")
			tc.mut(&n)
			err := n.Validate()
			if err == nil {
				t.Fatalf("Validate(%+v) = nil, want an error", n)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}
