package worker

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/gorizond/vcs-rag/internal/config"
	"github.com/gorizond/vcs-rag/internal/provider"
)

// startEmbeddedNATS starts an in-process NATS server with JetStream (T117/T119 tests).
func startEmbeddedNATS(t *testing.T) string {
	t.Helper()
	ns, err := natsserver.NewServer(&natsserver.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
		NoLog:     true,
		NoSigs:    true,
	})
	if err != nil {
		t.Fatalf("embedded nats: %v", err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(10 * time.Second) {
		t.Fatal("embedded nats is not ready")
	}
	t.Cleanup(ns.Shutdown)
	return ns.ClientURL()
}

// T117/T118: two instances share one stream but are isolated — a publish to a
// leaves b pending 0; the KV buckets differ; a wildcard (overlapping) filter is
// rejected; the topology is idempotent and is recreated after manual deletion.
func TestRefreshTopologyInstances(t *testing.T) {
	url := startEmbeddedNATS(t)
	namesA, namesB := config.RefreshNames("a"), config.RefreshNames("b")

	ncA, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer ncA.Close()
	ncB, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer ncB.Close()
	jsA, err := ncA.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	jsB, err := ncB.JetStream()
	if err != nil {
		t.Fatal(err)
	}

	// provisioning is idempotent (a second start is a no-op).
	for i := 0; i < 2; i++ {
		if err := ensureRefreshTopology(jsA, namesA); err != nil {
			t.Fatalf("ensure a (%d): %v", i, err)
		}
		if err := ensureRefreshTopology(jsB, namesB); err != nil {
			t.Fatalf("ensure b (%d): %v", i, err)
		}
	}

	si, err := jsA.StreamInfo(config.NATSStream)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(si.Config.Subjects) != 1 || si.Config.Subjects[0] != config.NATSSubjects {
		t.Errorf("stream subjects = %v, want the shared %q", si.Config.Subjects, config.NATSSubjects)
	}
	if si.Config.Retention != nats.WorkQueuePolicy || si.Config.Storage != nats.FileStorage {
		t.Errorf("stream retention/storage = %v/%v", si.Config.Retention, si.Config.Storage)
	}
	ciA, err := jsA.ConsumerInfo(config.NATSStream, namesA.Consumer)
	if err != nil {
		t.Fatalf("consumer a: %v", err)
	}
	if _, err := jsA.ConsumerInfo(config.NATSStream, namesB.Consumer); err != nil {
		t.Fatalf("consumer b: %v", err)
	}
	if ciA.Config.FilterSubject != namesA.Subject || ciA.Config.MaxAckPending != 1 ||
		ciA.Config.MaxDeliver != refreshMaxDeliver || ciA.Config.AckWait != refreshBackoff[0] ||
		len(ciA.Config.BackOff) != 2 {
		t.Errorf("consumer a config = %+v", ciA.Config)
	}

	// publish to a → pending only for a (the exact filters do not overlap).
	task, _ := json.Marshal(refreshTask{Repo: "grp/fixture", RequestID: "r1", TS: 1})
	if _, err := jsA.Publish(namesA.Subject, task); err != nil {
		t.Fatalf("publish: %v", err)
	}
	ciA, err = jsA.ConsumerInfo(config.NATSStream, namesA.Consumer)
	if err != nil {
		t.Fatal(err)
	}
	ciB, err := jsA.ConsumerInfo(config.NATSStream, namesB.Consumer)
	if err != nil {
		t.Fatal(err)
	}
	if ciA.NumPending != 1 {
		t.Errorf("pending a = %d, want 1", ciA.NumPending)
	}
	if ciB.NumPending != 0 {
		t.Errorf("pending b = %d, want 0 (the message is not for b)", ciB.NumPending)
	}

	// the KV buckets are isolated: a value of a is invisible in b and vice versa.
	kvA, err := jsA.KeyValue(namesA.KVBucket)
	if err != nil {
		t.Fatal(err)
	}
	kvB, err := jsB.KeyValue(namesB.KVBucket)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kvA.Put("key", []byte("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := kvB.Get("key"); err == nil {
		t.Error("KV b sees the key of a")
	}
	if _, err := kvB.Put("key2", []byte("b")); err != nil {
		t.Fatal(err)
	}
	if _, err := kvA.Get("key2"); err == nil {
		t.Error("KV a sees the key of b")
	}

	// an overlapping/wildcard filter is rejected up front by Validate...
	bad := namesA
	bad.Subject = config.NATSSubjects // vcsrag.*.refresh
	if err := ensureRefreshTopology(jsA, bad); err == nil {
		t.Fatal("a wildcard filter was accepted")
	}
	// ...and by the server itself: a WorkQueue stream refuses overlapping
	// consumers ("filtered consumer not unique on workqueue stream").
	if _, err := jsA.AddConsumer(config.NATSStream, &nats.ConsumerConfig{
		Durable: "overlap", FilterSubject: config.NATSSubjects, AckPolicy: nats.AckExplicitPolicy,
	}); err == nil {
		t.Fatal("the server accepted an overlapping consumer filter")
	}

	// a manual stream deletion → the next ensure recreates the stream and consumers.
	if err := jsA.DeleteStream(config.NATSStream); err != nil {
		t.Fatalf("delete stream: %v", err)
	}
	if err := ensureRefreshTopology(jsA, namesA); err != nil {
		t.Fatalf("recreate a: %v", err)
	}
	if err := ensureRefreshTopology(jsB, namesB); err != nil {
		t.Fatalf("recreate b: %v", err)
	}
	if _, err := jsA.StreamInfo(config.NATSStream); err != nil {
		t.Fatalf("stream after recreate: %v", err)
	}
	if _, err := jsA.ConsumerInfo(config.NATSStream, namesB.Consumer); err != nil {
		t.Fatalf("consumer b after recreate: %v", err)
	}
}

// --- consumer logic with fakes (T112) ---

type fakeKV struct {
	mu     sync.Mutex
	m      map[string][]byte
	putErr error
}

func newFakeKV() *fakeKV { return &fakeKV{m: make(map[string][]byte)} }

func (f *fakeKV) Get(key string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v, ok := f.m[key]; ok {
		return v, nil
	}
	return nil, nats.ErrKeyNotFound
}

func (f *fakeKV) Put(key string, value []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.putErr != nil {
		return f.putErr
	}
	f.m[key] = value
	return nil
}

type fakeMsg struct {
	data      []byte
	delivered uint64
	acks      int
	naks      int
	terms     int
}

func (m *fakeMsg) Data() []byte         { return m.data }
func (m *fakeMsg) NumDelivered() uint64 { return m.delivered }
func (m *fakeMsg) Ack() error           { m.acks++; return nil }
func (m *fakeMsg) Nak() error           { m.naks++; return nil }
func (m *fakeMsg) Term() error          { m.terms++; return nil }

// newTestConsumer builds a consumer over fakes: resolution always succeeds.
func newTestConsumer(kv refreshKV, run func(ctx context.Context, repo provider.Repo) RefreshResult) *refreshConsumer {
	repo := provider.Repo{ID: "1", Namespace: "grp", Path: "fixture"}
	return &refreshConsumer{
		kv:      kv,
		resolve: func(string) (provider.Repo, error) { return repo, nil },
		run:     run,
		logger:  testLogger(),
	}
}

func taskMsg(t *testing.T, repo string) []byte {
	t.Helper()
	data, err := json.Marshal(refreshTask{Repo: repo, RequestID: "r1", TS: 1})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestRefreshConsumerCoalesces(t *testing.T) {
	ctx := context.Background()
	kv := newFakeKV()
	runs := 0
	c := newTestConsumer(kv, func(context.Context, provider.Repo) RefreshResult {
		runs++
		return RefreshResult{CommitsIndexed: 2}
	})

	// a double publish → one run; the second message is coalesced (fresh done).
	first := &fakeMsg{data: taskMsg(t, "grp/fixture"), delivered: 1}
	c.handle(ctx, first)
	second := &fakeMsg{data: taskMsg(t, "grp/fixture"), delivered: 1}
	c.handle(ctx, second)
	if runs != 1 {
		t.Errorf("runs = %d, want 1 (the second message is coalesced)", runs)
	}
	if first.acks != 1 || second.acks != 1 {
		t.Errorf("acks = %d/%d, want 1/1", first.acks, second.acks)
	}
	if c.stats.coalesced.Load() != 1 {
		t.Errorf("coalesced = %d, want 1", c.stats.coalesced.Load())
	}

	// a fresh in_progress of a live run is a no-op too.
	live := refreshStatus{State: "in_progress", StartedAt: time.Now().UTC()}
	data, _ := json.Marshal(live)
	_ = kv.Put(repoSlug("grp/fixture"), data)
	third := &fakeMsg{data: taskMsg(t, "grp/fixture"), delivered: 1}
	c.handle(ctx, third)
	if runs != 1 || third.acks != 1 {
		t.Errorf("live in_progress: runs = %d, acks = %d, want 1/1", runs, third.acks)
	}

	// a stale in_progress (an earlier crash, older than the coalesce window =
	// the effective ack wait) → the redelivery proceeds (the checkpointed
	// pipelines make it duplicate-safe).
	stale := refreshStatus{State: "in_progress", StartedAt: time.Now().UTC().Add(-1 * time.Minute)}
	data, _ = json.Marshal(stale)
	_ = kv.Put(repoSlug("grp/fixture"), data)
	fourth := &fakeMsg{data: taskMsg(t, "grp/fixture"), delivered: 2}
	c.handle(ctx, fourth)
	if runs != 2 || fourth.acks != 1 {
		t.Errorf("stale in_progress: runs = %d, acks = %d, want 2/1", runs, fourth.acks)
	}
}

func TestRefreshConsumerFailures(t *testing.T) {
	ctx := context.Background()

	// a partial failure before MaxDeliver → Nak (retry) with the KV failed kept.
	kv := newFakeKV()
	c := newTestConsumer(kv, func(context.Context, provider.Repo) RefreshResult {
		return RefreshResult{CommitsIndexed: 1, Errors: []string{"metadata: boom"}}
	})
	msg := &fakeMsg{data: taskMsg(t, "grp/fixture"), delivered: 1}
	c.handle(ctx, msg)
	if msg.naks != 1 || msg.acks != 0 || msg.terms != 0 {
		t.Errorf("retry: nak/ack/term = %d/%d/%d, want 1/0/0", msg.naks, msg.acks, msg.terms)
	}
	var st refreshStatus
	if data, err := kv.Get(repoSlug("grp/fixture")); err != nil {
		t.Fatal(err)
	} else if err := json.Unmarshal(data, &st); err != nil || st.State != "failed" {
		t.Errorf("KV state = %q (err %v), want failed", st.State, err)
	}

	// at MaxDeliver → Term (terminal) with the KV failed.
	kv2 := newFakeKV()
	c2 := newTestConsumer(kv2, func(context.Context, provider.Repo) RefreshResult {
		return RefreshResult{Errors: []string{"index: boom"}}
	})
	last := &fakeMsg{data: taskMsg(t, "grp/fixture"), delivered: refreshMaxDeliver}
	c2.handle(ctx, last)
	if last.terms != 1 || last.naks != 0 || last.acks != 0 {
		t.Errorf("MaxDeliver: nak/ack/term = %d/%d/%d, want 0/0/1", last.naks, last.acks, last.terms)
	}
	if data, err := kv2.Get(repoSlug("grp/fixture")); err != nil {
		t.Fatal(err)
	} else if err := json.Unmarshal(data, &st); err != nil || st.State != "failed" {
		t.Errorf("KV state = %q, want failed", st.State)
	}

	// a transient KV failure before the run → Nak, the runner is not called.
	kv3 := newFakeKV()
	kv3.putErr = context.DeadlineExceeded
	runs := 0
	c3 := newTestConsumer(kv3, func(context.Context, provider.Repo) RefreshResult {
		runs++
		return RefreshResult{}
	})
	transient := &fakeMsg{data: taskMsg(t, "grp/fixture"), delivered: 1}
	c3.handle(ctx, transient)
	if transient.naks != 1 || runs != 0 {
		t.Errorf("transient: naks = %d, runs = %d, want 1/0", transient.naks, runs)
	}

	// an unparseable task is terminal.
	bad := &fakeMsg{data: []byte("{not json"), delivered: 1}
	c3.handle(ctx, bad)
	if bad.terms != 1 {
		t.Errorf("bad task: terms = %d, want 1", bad.terms)
	}

	// an empty discovery snapshot is transient (Nak), not terminal.
	c4 := &refreshConsumer{
		kv:      newFakeKV(),
		resolve: func(string) (provider.Repo, error) { return provider.Repo{}, errRefreshTransient },
		run:     func(context.Context, provider.Repo) RefreshResult { runs++; return RefreshResult{} },
		logger:  testLogger(),
	}
	empty := &fakeMsg{data: taskMsg(t, "grp/fixture"), delivered: 1}
	c4.handle(ctx, empty)
	if empty.naks != 1 || empty.terms != 0 {
		t.Errorf("empty snapshot: nak/term = %d/%d, want 1/0", empty.naks, empty.terms)
	}
}

// T112 end-to-end over embedded NATS: publish → fetch → run → KV done → ack.
func TestRefreshConsumerE2E(t *testing.T) {
	url := startEmbeddedNATS(t)
	names := config.RefreshNames("e2e")
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := nc.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureRefreshTopology(js, names); err != nil {
		t.Fatal(err)
	}
	sub, err := js.PullSubscribe(names.Subject, names.Consumer)
	if err != nil {
		t.Fatal(err)
	}
	kv, err := js.KeyValue(names.KVBucket)
	if err != nil {
		t.Fatal(err)
	}

	ran := make(chan struct{})
	c := &refreshConsumer{
		source: natsSource{sub},
		kv:     natsKV{kv},
		resolve: func(string) (provider.Repo, error) {
			return provider.Repo{ID: "1", Namespace: "grp", Path: "fixture"}, nil
		},
		run: func(context.Context, provider.Repo) RefreshResult {
			close(ran)
			return RefreshResult{CommitsIndexed: 2, MetaUpdated: 1}
		},
		logger: testLogger(),
	}

	if _, err := js.Publish(names.Subject, taskMsg(t, "grp/fixture")); err != nil {
		t.Fatalf("publish: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- c.consume(ctx) }()

	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("the consumer did not process the task")
	}
	// the KV status is written before the ack — wait for done.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if entry, err := kv.Get(repoSlug("grp/fixture")); err == nil {
			var st refreshStatus
			if json.Unmarshal(entry.Value(), &st) == nil && st.State == "done" && st.CommitsIndexed == 2 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the KV status is not done")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// the ack must release the message: the consumer has no pending left.
	deadline = time.Now().Add(3 * time.Second)
	for {
		ci, err := js.ConsumerInfo(config.NATSStream, names.Consumer)
		if err == nil && ci.NumPending == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the message was not acked (pending = %d)", ci.NumPending)
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-stopped:
		if err != nil {
			t.Errorf("consume: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the consumer did not stop")
	}
}

// The a/b isolation of the real adapters: a message of a is never delivered to b.
func TestRefreshConsumerIsolationAB(t *testing.T) {
	url := startEmbeddedNATS(t)
	namesA, namesB := config.RefreshNames("a"), config.RefreshNames("b")
	ncA, _ := nats.Connect(url)
	defer ncA.Close()
	ncB, _ := nats.Connect(url)
	defer ncB.Close()
	jsA, _ := ncA.JetStream()
	jsB, _ := ncB.JetStream()
	if err := ensureRefreshTopology(jsA, namesA); err != nil {
		t.Fatal(err)
	}
	if err := ensureRefreshTopology(jsB, namesB); err != nil {
		t.Fatal(err)
	}
	subB, err := jsB.PullSubscribe(namesB.Subject, namesB.Consumer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jsA.Publish(namesA.Subject, taskMsg(t, "grp/fixture")); err != nil {
		t.Fatal(err)
	}
	if _, err := subB.Fetch(1, nats.MaxWait(300*time.Millisecond)); err == nil {
		t.Fatal("b received the message of a")
	} else if !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("b fetch = %v, want a timeout", err)
	}
}
