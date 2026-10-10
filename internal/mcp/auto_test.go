package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/gorizond/vcs-rag/internal/config"
)

// fakeAuto is a controllable autoRefresher double.
type fakeAuto struct {
	mu        sync.Mutex
	statuses  map[string]refreshStatus
	waitDone  bool
	triggers  []string
	waits     []autoWaitCall
	triggerN  int
	triggerEr error
}

type autoWaitCall struct {
	repo string
	want string
	wait time.Duration
}

func (f *fakeAuto) Status(repo string) (refreshStatus, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.statuses[repo]
	return st, ok
}

func (f *fakeAuto) Wait(_ context.Context, repo, want string, wait time.Duration) bool {
	f.mu.Lock()
	f.waits = append(f.waits, autoWaitCall{repo: repo, want: want, wait: wait})
	done := f.waitDone
	f.mu.Unlock()
	return done
}

func (f *fakeAuto) Trigger(repo string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.triggerEr != nil {
		return "", f.triggerEr
	}
	f.triggerN++
	f.triggers = append(f.triggers, repo)
	return fmt.Sprintf("id-%d", f.triggerN), nil
}

func (f *fakeAuto) triggerCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.triggers)
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newAutoTools assembles tools with the auto policy over a fake client.
func newAutoTools(fake *fakeAuto) *tools {
	return &tools{embedder: &mockEmbedder{}, searcher: &mockSearcher{}, auto: newAutoClient(fake, discardLog())}
}

// T134: a fresh "done" (<10m) skips the auto-refresh entirely.
func TestAutoFreshDoneSkips(t *testing.T) {
	base := time.Unix(1000, 0)
	fake := &fakeAuto{statuses: map[string]refreshStatus{
		"g/nats": {State: "done", FinishedAt: base.Add(-time.Minute)},
	}, waitDone: true}
	tl := newAutoTools(fake)
	tl.auto.now = func() time.Time { return base }

	tl.auto.beforeSearch(context.Background(), "nats", []string{"g/nats"})
	if fake.triggerCount() != 0 || len(fake.waits) != 0 {
		t.Errorf("fresh done: triggers=%d waits=%d, want 0/0", fake.triggerCount(), len(fake.waits))
	}
	if tl.auto.stats.fresh != 1 || tl.auto.stats.checked != 1 {
		t.Errorf("stats = %+v", tl.auto.stats)
	}
}

// T134: a live "in_progress" (<15m) is awaited (≤3s) without a publication.
func TestAutoLiveRunAwaited(t *testing.T) {
	base := time.Unix(1000, 0)
	fake := &fakeAuto{statuses: map[string]refreshStatus{
		"g/nats": {State: "in_progress", StartedAt: base.Add(-5 * time.Minute)},
	}, waitDone: true}
	tl := newAutoTools(fake)
	tl.auto.now = func() time.Time { return base }

	tl.auto.beforeSearch(context.Background(), "nats", []string{"g/nats"})
	if fake.triggerCount() != 0 {
		t.Errorf("live run: triggers=%d, want 0", fake.triggerCount())
	}
	if len(fake.waits) != 1 {
		t.Fatalf("live run: waits=%d, want 1", len(fake.waits))
	}
	w := fake.waits[0]
	if w.want != "" || w.wait <= 2*time.Second || w.wait > autoWaitMax {
		t.Errorf("live wait = %+v, want want=\"\" and ~%v", w, autoWaitMax)
	}
	if tl.auto.stats.waitDone != 1 || tl.auto.stats.published != 0 {
		t.Errorf("stats = %+v", tl.auto.stats)
	}
}

// T134: no status → publish an auto task, then wait for its request id.
func TestAutoPublishesAndWaitsCorrelated(t *testing.T) {
	fake := &fakeAuto{statuses: map[string]refreshStatus{}, waitDone: true}
	tl := newAutoTools(fake)

	tl.auto.beforeSearch(context.Background(), "nats", []string{"g/nats"})
	if fake.triggerCount() != 1 || fake.triggers[0] != "g/nats" {
		t.Fatalf("triggers = %v, want [g/nats]", fake.triggers)
	}
	if len(fake.waits) != 1 || fake.waits[0].want != "id-1" || fake.waits[0].repo != "g/nats" {
		t.Errorf("waits = %+v, want want=id-1", fake.waits)
	}
	if tl.auto.stats.published != 1 || tl.auto.stats.waitDone != 1 {
		t.Errorf("stats = %+v", tl.auto.stats)
	}

	// wait timeout is counted separately and never fails the search.
	fake2 := &fakeAuto{statuses: map[string]refreshStatus{}, waitDone: false}
	tl2 := newAutoTools(fake2)
	tl2.auto.beforeSearch(context.Background(), "nats", []string{"g/nats"})
	if tl2.auto.stats.waitTimeout != 1 || tl2.auto.stats.waitDone != 0 {
		t.Errorf("timeout stats = %+v", tl2.auto.stats)
	}
}

// T134: ambiguous/not-found resolutions skip and are counted.
func TestAutoAmbiguousSkips(t *testing.T) {
	fake := &fakeAuto{statuses: map[string]refreshStatus{}}
	tl := newAutoTools(fake)

	tl.auto.beforeSearch(context.Background(), "nats", []string{"a/x/nats", "b/y/nats"})
	tl.auto.beforeSearch(context.Background(), "zzz", nil)
	if fake.triggerCount() != 0 {
		t.Errorf("triggers = %d, want 0", fake.triggerCount())
	}
	if tl.auto.stats.suppressedAmbiguous != 2 {
		t.Errorf("suppressed_ambiguous = %d, want 2", tl.auto.stats.suppressedAmbiguous)
	}
}

// T134: the per-repo interval (5m) suppresses a repeat publication.
func TestAutoSuppressedInterval(t *testing.T) {
	base := time.Unix(1000, 0)
	fake := &fakeAuto{statuses: map[string]refreshStatus{}}
	tl := newAutoTools(fake)
	tl.auto.now = func() time.Time { return base }
	tl.auto.rnd = func() float64 { return 0 } // no jitter pause

	tl.auto.beforeSearch(context.Background(), "nats", []string{"g/nats"})
	if fake.triggerCount() != 1 {
		t.Fatalf("first call triggers = %d, want 1", fake.triggerCount())
	}
	base = base.Add(time.Minute)
	tl.auto.beforeSearch(context.Background(), "nats", []string{"g/nats"})
	if fake.triggerCount() != 1 {
		t.Errorf("repeat within 5m triggers = %d, want 1", fake.triggerCount())
	}
	if tl.auto.stats.suppressedInterval != 1 {
		t.Errorf("suppressed_interval = %d, want 1", tl.auto.stats.suppressedInterval)
	}
	base = base.Add(5 * time.Minute)
	tl.auto.beforeSearch(context.Background(), "nats", []string{"g/nats"})
	if fake.triggerCount() != 2 {
		t.Errorf("after 6m triggers = %d, want 2", fake.triggerCount())
	}
}

// T134: the fixed-window global buckets — 6/min and 60/h.
func TestAutoSuppressedBucket(t *testing.T) {
	base := time.Unix(1000, 0)
	fake := &fakeAuto{statuses: map[string]refreshStatus{}}
	tl := newAutoTools(fake)
	tl.auto.now = func() time.Time { return base }
	tl.auto.rnd = func() float64 { return 0 }

	for i := 0; i < 6; i++ {
		tl.auto.beforeSearch(context.Background(), fmt.Sprintf("r%d", i), []string{fmt.Sprintf("g/r%d", i)})
	}
	tl.auto.beforeSearch(context.Background(), "r6", []string{"g/r6"})
	if fake.triggerCount() != 6 {
		t.Errorf("triggers = %d, want 6 (the 7th is suppressed)", fake.triggerCount())
	}
	if tl.auto.stats.suppressedBucket != 1 {
		t.Errorf("suppressed_bucket = %d, want 1", tl.auto.stats.suppressedBucket)
	}

	// the next minute refills the minute window, but the hour budget still holds.
	base = base.Add(time.Minute)
	for i := 0; i < 6; i++ {
		tl.auto.beforeSearch(context.Background(), fmt.Sprintf("m%d", i), []string{fmt.Sprintf("g/m%d", i)})
	}
	tl.auto.beforeSearch(context.Background(), "m6", []string{"g/m6"})
	if fake.triggerCount() != 12 {
		t.Errorf("triggers after the next minute = %d, want 12", fake.triggerCount())
	}
	if tl.auto.stats.suppressedBucket != 2 {
		t.Errorf("suppressed_bucket = %d, want 2", tl.auto.stats.suppressedBucket)
	}
}

// T134: the publication jitter is bounded (U[0,300ms]) and trims the wait
// budget, so the extra latency never exceeds autoWaitMax.
func TestAutoJitterAndBudget(t *testing.T) {
	fake := &fakeAuto{statuses: map[string]refreshStatus{}, waitDone: true}
	tl := newAutoTools(fake)
	tl.auto.jitter = 40 * time.Millisecond
	tl.auto.rnd = func() float64 { return 1 } // the maximum jitter

	tl.auto.beforeSearch(context.Background(), "nats", []string{"g/nats"})
	if len(fake.waits) != 1 {
		t.Fatalf("waits = %d, want 1", len(fake.waits))
	}
	// 40ms of the 3s budget were spent on the jitter pause.
	if got := fake.waits[0].wait; got <= 0 || got > autoWaitMax-20*time.Millisecond {
		t.Errorf("wait budget = %v, want (0, %v]", got, autoWaitMax-20*time.Millisecond)
	}
}

// T134: the INFO summary is emitted at most once per 5 minutes.
func TestAutoSummaryLog(t *testing.T) {
	base := time.Unix(1000, 0)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	fake := &fakeAuto{statuses: map[string]refreshStatus{}}
	cl := newAutoClient(fake, logger)
	cl.now = func() time.Time { return base }
	cl.rnd = func() float64 { return 0 }
	cl.lastLog = base

	for i := 0; i < 7; i++ { // 6 published + 1 suppressed by the bucket
		cl.beforeSearch(context.Background(), fmt.Sprintf("r%d", i), []string{fmt.Sprintf("g/r%d", i)})
	}
	if strings.Contains(buf.String(), "auto-refresh summary") {
		t.Fatalf("summary logged before 5m: %s", buf.String())
	}

	base = base.Add(5*time.Minute + time.Second)
	cl.beforeSearch(context.Background(), "zzz", nil)
	out := buf.String()
	if !strings.Contains(out, "auto-refresh summary") ||
		!strings.Contains(out, "suppressed_bucket=1") || !strings.Contains(out, "published=6") {
		t.Errorf("summary = %s", out)
	}
}

// T134: the wire contract of the auto task and the KV correlation, on an
// embedded NATS.
func TestAutoTriggerCorrelationAndOrigin(t *testing.T) {
	url := startEmbeddedNATS(t)
	names := config.RefreshNames("auto")
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

	// Trigger publishes an origin="auto" task with the request id in the header.
	sub, err := js.PullSubscribe(names.Subject, names.Consumer)
	if err != nil {
		t.Fatal(err)
	}
	id, err := ref.Trigger(repo)
	if err != nil {
		t.Fatalf("trigger: %v", err)
	}
	msgs, err := sub.Fetch(1, nats.MaxWait(2*time.Second))
	if err != nil || len(msgs) != 1 {
		t.Fatalf("fetch auto task: err=%v msgs=%d", err, len(msgs))
	}
	if got := msgs[0].Header.Get("Nats-Msg-Id"); got != id {
		t.Errorf("Nats-Msg-Id = %q, want %q", got, id)
	}
	var task refreshTask
	if err := json.Unmarshal(msgs[0].Data, &task); err != nil {
		t.Fatal(err)
	}
	_ = msgs[0].Ack()
	if task.Origin != "auto" || task.Repo != repo || task.RequestID != id || task.TS == 0 {
		t.Errorf("task = %+v", task)
	}
	if !strings.Contains(string(msgs[0].Data), `"origin":"auto"`) {
		t.Errorf("task JSON has no origin: %s", msgs[0].Data)
	}

	// a stale done of another request is not accepted for the new id.
	putStatus(refreshStatus{State: "done", RequestID: "other", FinishedAt: time.Now().UTC()})
	if ref.Wait(context.Background(), repo, id, 300*time.Millisecond) {
		t.Error("Wait accepted a stale done of another request")
	}
	// the correlated done is accepted.
	putStatus(refreshStatus{State: "done", RequestID: id, FinishedAt: time.Now().UTC()})
	if !ref.Wait(context.Background(), repo, id, 2*time.Second) {
		t.Error("Wait did not see the correlated done")
	}
	if st, ok := ref.Status(repo); !ok || st.State != "done" || st.RequestID != id {
		t.Errorf("Status = %+v, %v", st, ok)
	}
	// want="" accepts any terminal status (an earlier live run).
	putStatus(refreshStatus{State: "done", RequestID: "earlier", FinishedAt: time.Now().UTC()})
	if !ref.Wait(context.Background(), repo, "", 2*time.Second) {
		t.Error("Wait(\"\") did not accept a terminal status")
	}
}
