package config

import (
	"fmt"
	"regexp"
	"strings"
)

// NATS refresh topology (T117). Several vcs-rag deployments share one NATS:
// the stream is common, while the consumer, request subject, KV bucket and
// connection name are scoped by the instance id. All names are built HERE —
// worker and MCP read them from config.
const (
	NATSStream          = "VCSRAG_REFRESH"   // shared WorkQueue stream
	NATSSubjects        = "vcsrag.*.refresh" // shared subject list of the stream
	NATSDefaultInstance = "default"          // NATS_INSTANCE_ID default
)

// instanceIDRe is the allowed NATS_INSTANCE_ID: a lowercase alphanumeric start,
// then lowercase alphanumerics, "_" or "-", up to 32 characters.
var instanceIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// RefreshTopology are the NATS object names of one instance (T117).
type RefreshTopology struct {
	InstanceID string
	Stream     string
	Subjects   []string // shared stream subject list (instance-independent)
	Subject    string   // instance request subject: vcsrag.<id>.refresh
	Consumer   string   // instance pull consumer: worker-refresh-<id>
	KVBucket   string   // instance status KV: refresh_status_<id>
	// WorkerKVBucket is the indexing status KV (T150): the worker periodically
	// publishes its indexing snapshot there, the MCP index_status tool reads it.
	WorkerKVBucket string // worker_status_<id>
	ConnName       string // connection name: vcs-rag-<id>
}

// RefreshNames builds the names of an instance; an empty id means the default
// instance.
func RefreshNames(id string) RefreshTopology {
	if id == "" {
		id = NATSDefaultInstance
	}
	return RefreshTopology{
		InstanceID:     id,
		Stream:         NATSStream,
		Subjects:       []string{NATSSubjects},
		Subject:        "vcsrag." + id + ".refresh",
		Consumer:       "worker-refresh-" + id,
		KVBucket:       "refresh_status_" + id,
		WorkerKVBucket: "worker_status_" + id,
		ConnName:       "vcs-rag-" + id,
	}
}

// Validate rejects a malformed instance id and any wildcard/overlapping
// filter: the consumer must match exactly the instance subject, otherwise two
// instances would steal each other's tasks on the shared WorkQueue stream.
func (n RefreshTopology) Validate() error {
	want := RefreshNames(n.InstanceID)
	if !instanceIDRe.MatchString(n.InstanceID) {
		return fmt.Errorf("nats: invalid instance id %q (want %s)", n.InstanceID, instanceIDRe)
	}
	if n.Stream != want.Stream || len(n.Subjects) != 1 || n.Subjects[0] != want.Subjects[0] {
		return fmt.Errorf("nats: stream %q/%v must be the shared %q/%v",
			n.Stream, n.Subjects, want.Stream, want.Subjects)
	}
	if n.Subject != want.Subject || strings.ContainsAny(n.Subject, "*>") {
		return fmt.Errorf("nats: consumer filter %q must be the exact instance subject %q", n.Subject, want.Subject)
	}
	if n.Consumer != want.Consumer || strings.ContainsAny(n.Consumer, "*>") {
		return fmt.Errorf("nats: consumer %q must be %q", n.Consumer, want.Consumer)
	}
	if n.KVBucket != want.KVBucket {
		return fmt.Errorf("nats: KV bucket %q must be %q", n.KVBucket, want.KVBucket)
	}
	return nil
}
