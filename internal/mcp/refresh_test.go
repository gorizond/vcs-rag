package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/gorizond/vcs-rag/internal/config"
)

// fakeRefresher — a refresher double: records the timeout, counts calls.
type fakeRefresher struct {
	out     refreshOutcome
	err     error
	delay   time.Duration
	calls   int
	timeout time.Duration
}

func (f *fakeRefresher) Refresh(ctx context.Context, _ string, timeout time.Duration) (refreshOutcome, error) {
	f.calls++
	f.timeout = timeout
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return refreshOutcome{}, ctx.Err()
		}
	}
	return f.out, f.err
}

// T113: status=done (cached) path.
func TestRefreshRepoToolDone(t *testing.T) {
	fake := &fakeRefresher{out: refreshOutcome{
		Status: refreshStatus{State: "done", CommitsIndexed: 3, MetaUpdated: 1, DurationMS: 42},
		Cached: true, Seen: true,
	}}
	tl := newTools(&mockEmbedder{}, &mockSearcher{}, fake)

	_, out, err := tl.refreshRepo(context.Background(), nil, refreshRepoInput{Repo: "grp/fixture"})
	if err != nil {
		t.Fatalf("refreshRepo: %v", err)
	}
	resp, ok := out.(refreshDoneResponse)
	if !ok {
		t.Fatalf("out = %T, want refreshDoneResponse", out)
	}
	if resp.Status != "done" || !resp.Cached || resp.CommitsIndexed != 3 || resp.MetaUpdated != 1 {
		t.Errorf("response = %+v", resp)
	}
	js := marshalJSON(out)
	for _, want := range []string{`"status":"done"`, `"commits_indexed":3`, `"meta_updated":1`,
		`"duration_ms":42`, `"cached":true`, `"errors":[]`} {
		if !strings.Contains(js, want) {
			t.Errorf("JSON does not contain %s: %s", want, js)
		}
	}
}

// T113: status=in_progress after the poll timeout (the task continues in background).
func TestRefreshRepoToolInProgress(t *testing.T) {
	started := time.Now().UTC().Add(-3 * time.Second)
	fake := &fakeRefresher{out: refreshOutcome{
		Status: refreshStatus{State: "in_progress", StartedAt: started},
		Seen:   true,
	}}
	tl := newTools(&mockEmbedder{}, &mockSearcher{}, fake)

	_, out, err := tl.refreshRepo(context.Background(), nil, refreshRepoInput{Repo: "grp/fixture"})
	if err != nil {
		t.Fatalf("refreshRepo: %v", err)
	}
	resp, ok := out.(refreshInProgressResponse)
	if !ok {
		t.Fatalf("out = %T, want refreshInProgressResponse", out)
	}
	if resp.Status != "in_progress" || !resp.WorkerSeen || resp.StartedAt == "" || resp.ElapsedMS < 0 {
		t.Errorf("response = %+v", resp)
	}
	js := marshalJSON(out)
	for _, want := range []string{`"status":"in_progress"`, `"worker_seen":true`, `"started_at":"`} {
		if !strings.Contains(js, want) {
			t.Errorf("JSON does not contain %s: %s", want, js)
		}
	}

	// worker_seen=false: the key was never written (the worker did not pick the task up).
	fake2 := &fakeRefresher{out: refreshOutcome{Status: refreshStatus{State: "in_progress"}}}
	tl2 := newTools(&mockEmbedder{}, &mockSearcher{}, fake2)
	_, out2, _ := tl2.refreshRepo(context.Background(), nil, refreshRepoInput{Repo: "grp/fixture"})
	if r := out2.(refreshInProgressResponse); r.WorkerSeen {
		t.Errorf("worker_seen = true for an empty KV: %+v", r)
	}
}

// T113: NATS unavailable and terminal KV failed → the structured error.
func TestRefreshRepoToolUnavailable(t *testing.T) {
	tl := newTools(&mockEmbedder{}, &mockSearcher{}, &fakeRefresher{err: errors.New("nats: no servers available")})
	_, out, err := tl.refreshRepo(context.Background(), nil, refreshRepoInput{Repo: "grp/fixture"})
	if err != nil {
		t.Fatalf("protocol error: %v", err)
	}
	te, ok := out.(toolError)
	if !ok || te.Code != codeRefreshUnavailable {
		t.Fatalf("out = %#v, want refresh_unavailable", out)
	}
	if !strings.Contains(te.Message, "continue searching the current index") {
		t.Errorf("message = %q", te.Message)
	}

	// a terminally failed KV status carries the worker errors.
	failed := &fakeRefresher{out: refreshOutcome{Status: refreshStatus{
		State: "failed", Errors: []string{"mirror: connection refused"},
	}}}
	tl2 := newTools(&mockEmbedder{}, &mockSearcher{}, failed)
	_, out2, _ := tl2.refreshRepo(context.Background(), nil, refreshRepoInput{Repo: "grp/fixture"})
	te2 := out2.(toolError)
	if te2.Code != codeRefreshUnavailable || !strings.Contains(te2.Message, "mirror: connection refused") {
		t.Errorf("failed response = %+v", te2)
	}

	// an empty repo is rejected.
	tl3 := newTools(&mockEmbedder{}, &mockSearcher{}, &fakeRefresher{})
	if _, out3, _ := tl3.refreshRepo(context.Background(), nil, refreshRepoInput{Repo: "  "}); out3.(toolError).Code != codeRefreshUnavailable {
		t.Errorf("empty repo = %#v", out3)
	}

	// without NATS the tool is not registered: the handler refuses defensively.
	tl4 := newTools(&mockEmbedder{}, &mockSearcher{}, nil)
	if _, out4, _ := tl4.refreshRepo(context.Background(), nil, refreshRepoInput{Repo: "grp/fixture"}); out4.(toolError).Code != codeRefreshUnavailable {
		t.Errorf("disabled = %#v", out4)
	}
}

// T113: timeout_ms defaults to 20 s and is capped at 120 s.
func TestRefreshRepoTimeoutClamp(t *testing.T) {
	for _, tc := range []struct {
		in   int
		want time.Duration
	}{{0, 20 * time.Second}, {5000, 5 * time.Second}, {999999, 120 * time.Second}} {
		fake := &fakeRefresher{}
		tl := newTools(&mockEmbedder{}, &mockSearcher{}, fake)
		_, _, _ = tl.refreshRepo(context.Background(), nil, refreshRepoInput{Repo: "grp/fixture", TimeoutMS: tc.in})
		if fake.timeout != tc.want {
			t.Errorf("timeout_ms %d → %v, want %v", tc.in, fake.timeout, tc.want)
		}
	}
}

// T113: concurrent calls for one repository share a single in-flight run.
func TestRefreshRepoFlightShare(t *testing.T) {
	fake := &fakeRefresher{delay: 60 * time.Millisecond,
		out: refreshOutcome{Status: refreshStatus{State: "done", CommitsIndexed: 1}}}
	tl := newTools(&mockEmbedder{}, &mockSearcher{}, fake)

	results := make(chan refreshDoneResponse, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, out, _ := tl.refreshRepo(context.Background(), nil, refreshRepoInput{Repo: "grp/fixture"})
			results <- out.(refreshDoneResponse)
		}()
	}
	wg.Wait()
	close(results)
	if fake.calls != 1 {
		t.Errorf("refresher calls = %d, want 1 (in-flight share)", fake.calls)
	}
	n := 0
	for r := range results {
		n++
		if r.CommitsIndexed != 1 {
			t.Errorf("response = %+v", r)
		}
	}
	if n != 2 {
		t.Errorf("results = %d, want 2", n)
	}
}

// The wire contract: the read model parses the worker status JSON and the task
// carries the expected keys.
func TestRefreshWireContract(t *testing.T) {
	const sample = `{"state":"done","request_id":"abc","started_at":"2026-10-10T20:00:00Z",` +
		`"finished_at":"2026-10-10T20:00:05Z","commits_indexed":2,"meta_updated":1,` +
		`"errors":["metadata: boom"],"duration_ms":5000}`
	var st refreshStatus
	if err := json.Unmarshal([]byte(sample), &st); err != nil {
		t.Fatal(err)
	}
	if st.State != "done" || st.RequestID != "abc" || st.CommitsIndexed != 2 ||
		st.MetaUpdated != 1 || st.DurationMS != 5000 || len(st.Errors) != 1 ||
		st.StartedAt.IsZero() || st.FinishedAt.IsZero() {
		t.Errorf("status = %+v", st)
	}
	data, err := json.Marshal(refreshTask{Repo: "r", RequestID: "id", TS: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"repo":"r"`, `"request_id":"id"`, `"ts":1`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("task JSON does not contain %s: %s", want, data)
		}
	}
}

// startEmbeddedNATS starts an in-process NATS server with JetStream (T119 test).
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

// T113/T122: registration must not panic — with and without a refresher
// (the tool is added only when NATS_URL is set) and the jsonschema tags of all
// tools must be valid.
func TestRegisterRefreshTool(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	registerTools(server, newTools(&mockEmbedder{}, &mockSearcher{}, nil))
	registerTools(server, newTools(&mockEmbedder{}, &mockSearcher{}, &fakeRefresher{}))
}

// T122: the poll is correlated with the published request — a stale "done" of
// an earlier run is not returned for a new task; a "done" carrying the new
// request id is.
func TestNATSRefresherStaleDoneCorrelation(t *testing.T) {
	url := startEmbeddedNATS(t)
	names := config.RefreshNames("fix")
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := nc.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.AddStream(&nats.StreamConfig{
		Name: names.Stream, Subjects: names.Subjects, Retention: nats.WorkQueuePolicy, Storage: nats.FileStorage,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := js.AddConsumer(names.Stream, &nats.ConsumerConfig{
		Durable: names.Consumer, FilterSubject: names.Subject, AckPolicy: nats.AckExplicitPolicy, MaxAckPending: 1,
	}); err != nil {
		t.Fatal(err)
	}
	kv, err := js.CreateKeyValue(&nats.KeyValueConfig{Bucket: names.KVBucket, Storage: nats.FileStorage})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := connectRefreshClient(url, names)
	if err != nil {
		t.Fatal(err)
	}
	defer ref.Close()

	const repo = "grp/fixture"
	key := refreshSlug(repo)
	putStatus := func(st refreshStatus) {
		t.Helper()
		data, err := json.Marshal(st)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := kv.Put(key, data); err != nil {
			t.Fatal(err)
		}
	}

	// an earlier run left a stale "done" in the KV (older than the freshness window).
	putStatus(refreshStatus{State: "done", RequestID: "old", FinishedAt: time.Now().UTC().Add(-5 * time.Minute),
		CommitsIndexed: 7, DurationMS: 111})

	// (b) the source answers the new task with a done carrying its request id.
	sub, err := js.PullSubscribe(names.Subject, names.Consumer)
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan refreshOutcome, 1)
	errs := make(chan error, 1)
	go func() {
		out, err := ref.Refresh(context.Background(), repo, 3*time.Second)
		if err != nil {
			errs <- err
			return
		}
		results <- out
	}()
	msgs, err := sub.Fetch(1, nats.MaxWait(2*time.Second))
	if err != nil || len(msgs) != 1 {
		t.Fatalf("fetch the published task: err=%v msgs=%d", err, len(msgs))
	}
	var task refreshTask
	if err := json.Unmarshal(msgs[0].Data, &task); err != nil {
		t.Fatal(err)
	}
	_ = msgs[0].Ack()
	putStatus(refreshStatus{State: "done", RequestID: task.RequestID, FinishedAt: time.Now().UTC(),
		CommitsIndexed: 3, DurationMS: 42})
	select {
	case out := <-results:
		if out.Cached || out.Status.State != "done" || out.Status.RequestID != task.RequestID ||
			out.Status.CommitsIndexed != 3 {
			t.Errorf("(b) out = %+v, want the done of task %s", out, task.RequestID)
		}
	case err := <-errs:
		t.Fatalf("(b) refresh: %v", err)
	case <-time.After(6 * time.Second):
		t.Fatal("(b) refresh did not return")
	}

	// (a) a stale done and no source: the stale result must not be reported —
	// the timeout yields in_progress, not the numbers of the previous refresh.
	putStatus(refreshStatus{State: "done", RequestID: "old2", FinishedAt: time.Now().UTC().Add(-5 * time.Minute),
		CommitsIndexed: 9})
	out, err := ref.Refresh(context.Background(), repo, 400*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if out.Cached || out.Status.State != "in_progress" || out.Status.RequestID == "old2" ||
		out.Status.CommitsIndexed == 9 {
		t.Errorf("(a) stale done leaked: %+v", out)
	}
	ci, err := js.ConsumerInfo(names.Stream, names.Consumer)
	if err != nil {
		t.Fatal(err)
	}
	if ci.NumPending != 1 {
		t.Errorf("(a) pending = %d, want 1 (the new task was published)", ci.NumPending)
	}
}

// T119: the client publishes to the instance subject and reads the instance KV
// bucket; a cached "done" is served without a publication.
func TestNATSRefresherInstanceScope(t *testing.T) {
	url := startEmbeddedNATS(t)
	names := config.RefreshNames("a")
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := nc.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	// the worker side would provision these; here the test does it explicitly.
	if _, err := js.AddStream(&nats.StreamConfig{
		Name: names.Stream, Subjects: names.Subjects, Retention: nats.WorkQueuePolicy, Storage: nats.FileStorage,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := js.AddConsumer(names.Stream, &nats.ConsumerConfig{
		Durable: names.Consumer, FilterSubject: names.Subject, AckPolicy: nats.AckExplicitPolicy,
	}); err != nil {
		t.Fatal(err)
	}
	kv, err := js.CreateKeyValue(&nats.KeyValueConfig{Bucket: names.KVBucket, Storage: nats.FileStorage})
	if err != nil {
		t.Fatal(err)
	}

	ref, err := connectRefreshClient(url, names)
	if err != nil {
		t.Fatal(err)
	}
	defer ref.Close()

	// cached: a fresh "done" in the instance KV is served directly.
	done := refreshStatus{State: "done", FinishedAt: time.Now().UTC(), CommitsIndexed: 4, DurationMS: 9}
	data, _ := json.Marshal(done)
	if _, err := kv.Put(refreshSlug("grp/fixture"), data); err != nil {
		t.Fatal(err)
	}
	out, err := ref.Refresh(context.Background(), "grp/fixture", time.Second)
	if err != nil {
		t.Fatalf("cached refresh: %v", err)
	}
	if !out.Cached || out.Status.State != "done" || out.Status.CommitsIndexed != 4 {
		t.Errorf("cached outcome = %+v", out)
	}

	// publication: without a status the client publishes to the instance subject
	// and polls until the timeout (no worker runs here).
	if err := kv.Purge(refreshSlug("grp/fixture")); err != nil {
		t.Fatal(err)
	}
	out2, err := ref.Refresh(context.Background(), "grp/fixture", 300*time.Millisecond)
	if err != nil {
		t.Fatalf("publish refresh: %v", err)
	}
	if out2.Cached || out2.Status.State != "in_progress" || out2.Seen {
		t.Errorf("published outcome = %+v", out2)
	}
	ci, err := js.ConsumerInfo(names.Stream, names.Consumer)
	if err != nil {
		t.Fatal(err)
	}
	if ci.NumPending != 1 {
		t.Errorf("pending = %d, want 1 (the task landed on the instance subject)", ci.NumPending)
	}

	// the other instance has no such bucket — the isolation is by name.
	if _, err := js.KeyValue(config.RefreshNames("b").KVBucket); err == nil {
		t.Error("bucket b exists in the a-scoped test")
	}
}
