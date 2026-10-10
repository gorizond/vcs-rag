package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/gorizond/vcs-rag/internal/config"
	"github.com/gorizond/vcs-rag/internal/log"
	"github.com/gorizond/vcs-rag/internal/provider"
)

// Refresh consumer parameters (T109): fixed, visible in /jsz?streams=1&consumers=1.
// The object names are built per instance by config.RefreshNames (T117).
//
// nats-server ties the ack wait to the retry schedule: "If BackOff was
// specified that will override the AckWait" (server/consumer.go) — it sets
// AckWait = BackOff[0]. The design's AckWait=15m therefore cannot coexist with
// the [30s, 2m] backoff; the effective ack wait is 30s and the coalesce window
// below is aligned with it, so a redelivery after a crash is never swallowed
// by the guard (T112).
var refreshBackoff = []time.Duration{30 * time.Second, 2 * time.Minute}

const (
	refreshMaxDeliver     = 3
	refreshTTL            = 24 * time.Hour
	refreshCoalesceWindow = 30 * time.Second // effective ack wait = refreshBackoff[0]
	refreshDoneFresh      = 30 * time.Second // a "done" newer than this is coalesced
	refreshLaneBackoff    = 10 * time.Second // reconnect pause of the lane
)

// refreshTask is the published request (T112/T113 wire contract).
type refreshTask struct {
	Repo      string `json:"repo"`
	RequestID string `json:"request_id"`
	TS        int64  `json:"ts"`
}

// refreshKV is the KV surface used by the worker consumer (fake in tests).
type refreshKV interface {
	Get(key string) ([]byte, error) // nats.ErrKeyNotFound when absent
	Put(key string, value []byte) error
}

// natsKV adapts nats.KeyValue to refreshKV.
type natsKV struct{ kv nats.KeyValue }

func (a natsKV) Get(key string) ([]byte, error) {
	entry, err := a.kv.Get(key)
	if err != nil {
		return nil, err
	}
	return entry.Value(), nil
}

func (a natsKV) Put(key string, value []byte) error {
	_, err := a.kv.Put(key, value)
	return err
}

// refreshMsg is the consumer-visible message (fake in tests).
type refreshMsg interface {
	Data() []byte
	NumDelivered() uint64
	Ack() error
	Nak() error
	Term() error
}

// refreshSource delivers messages (a pull subscription in production, T112).
type refreshSource interface {
	Fetch(ctx context.Context, batch int, timeout time.Duration) ([]refreshMsg, error)
}

// natsSource adapts *nats.Subscription to refreshSource.
type natsSource struct{ sub *nats.Subscription }

// Fetch uses MaxWait only: nats.go rejects Context+MaxWait together. The
// shutdown latency is bounded by the poll timeout.
func (s natsSource) Fetch(_ context.Context, batch int, timeout time.Duration) ([]refreshMsg, error) {
	msgs, err := s.sub.Fetch(batch, nats.MaxWait(timeout))
	if err != nil {
		return nil, err
	}
	out := make([]refreshMsg, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, natsMsg{m})
	}
	return out, nil
}

// natsMsg adapts *nats.Msg to refreshMsg.
type natsMsg struct{ m *nats.Msg }

func (m natsMsg) Data() []byte { return m.m.Data }

func (m natsMsg) NumDelivered() uint64 {
	if md, err := m.m.Metadata(); err == nil && md != nil {
		return md.NumDelivered
	}
	return 1
}

func (m natsMsg) Ack() error  { return m.m.Ack() }
func (m natsMsg) Nak() error  { return m.m.Nak() }
func (m natsMsg) Term() error { return m.m.Term() }

// connectNATS dials JetStream with reconnect (T108/T117). The URL is never
// logged (it may carry credentials); the connection name carries the instance id.
func connectNATS(url, connName string, logger *slog.Logger) (*nats.Conn, error) {
	nc, err := nats.Connect(url,
		nats.Name(connName),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("nats: connect: %w", err)
	}
	logger.Info("NATS connected", "instance", connName)
	return nc, nil
}

// ensureRefreshTopology idempotently creates or reconciles the shared stream,
// the instance pull consumer and the instance KV bucket (T109/T117/T118); a
// manually deleted object is recreated on the next start. Only exact
// per-instance filters are accepted — an overlapping/wildcard filter would let
// instances steal each other's tasks and is rejected up front.
func ensureRefreshTopology(js nats.JetStreamContext, names config.RefreshTopology) error {
	if err := names.Validate(); err != nil {
		return err
	}
	if err := ensureStream(js, names, &nats.StreamConfig{
		Name:      names.Stream,
		Subjects:  names.Subjects, // shared: vcsrag.*.refresh
		Retention: nats.WorkQueuePolicy,
		Storage:   nats.FileStorage,
		MaxAge:    refreshTTL,
	}); err != nil {
		return err
	}
	if err := ensureConsumer(js, names, &nats.ConsumerConfig{
		Durable:       names.Consumer,
		FilterSubject: names.Subject, // exact instance filter
		AckPolicy:     nats.AckExplicitPolicy,
		AckWait:       refreshBackoff[0], // explicit: the server ties it to BackOff[0]
		MaxAckPending: 1,                 // serialization: one refresh at a time (LM care)
		MaxDeliver:    refreshMaxDeliver,
		BackOff:       refreshBackoff,
	}); err != nil {
		return err
	}
	// ponytail: the legacy KV API has no in-place config update; an existing
	// bucket is reused as is (TTL is fixed at creation).
	kvCfg := &nats.KeyValueConfig{Bucket: names.KVBucket, TTL: refreshTTL, Storage: nats.FileStorage}
	if _, err := js.KeyValue(names.KVBucket); errors.Is(err, nats.ErrBucketNotFound) {
		if _, err := js.CreateKeyValue(kvCfg); err != nil {
			return fmt.Errorf("nats: create KV %s: %w", names.KVBucket, err)
		}
	} else if err != nil {
		return fmt.Errorf("nats: KV %s: %w", names.KVBucket, err)
	}
	return nil
}

// ensureStream creates the stream or reconciles an existing one.
func ensureStream(js nats.JetStreamContext, names config.RefreshTopology, cfg *nats.StreamConfig) error {
	_, err := js.StreamInfo(cfg.Name)
	switch {
	case errors.Is(err, nats.ErrStreamNotFound):
		if _, err := js.AddStream(cfg); err != nil {
			return fmt.Errorf("nats: create stream %s: %w", cfg.Name, err)
		}
	case err != nil:
		return fmt.Errorf("nats: stream %s: %w", cfg.Name, err)
	default:
		if _, err := js.UpdateStream(cfg); err != nil {
			return fmt.Errorf("nats: update stream %s: %w", cfg.Name, err)
		}
	}
	return nil
}

// ensureConsumer creates the durable pull consumer or reconciles an existing one.
func ensureConsumer(js nats.JetStreamContext, names config.RefreshTopology, cfg *nats.ConsumerConfig) error {
	_, err := js.ConsumerInfo(names.Stream, cfg.Durable)
	switch {
	case errors.Is(err, nats.ErrConsumerNotFound):
		if _, err := js.AddConsumer(names.Stream, cfg); err != nil {
			return fmt.Errorf("nats: create consumer %s: %w", cfg.Durable, err)
		}
	case err != nil:
		return fmt.Errorf("nats: consumer %s: %w", cfg.Durable, err)
	default:
		if _, err := js.UpdateConsumer(names.Stream, cfg); err != nil {
			return fmt.Errorf("nats: update consumer %s: %w", cfg.Durable, err)
		}
	}
	return nil
}

// refreshStats — per-lane counters (T112).
type refreshStats struct {
	received  atomic.Int64
	coalesced atomic.Int64
	ok        atomic.Int64
	failed    atomic.Int64
}

// refreshConsumer processes vcsrag.<id>.refresh messages: a coalesce guard by
// KV, a refresh execution and the status write; the ack happens strictly after
// the status write (T109 invariant).
type refreshConsumer struct {
	source  refreshSource
	kv      refreshKV
	resolve func(id string) (provider.Repo, error)
	run     func(ctx context.Context, repo provider.Repo) RefreshResult
	logger  *slog.Logger
	stats   refreshStats
}

// newRefreshConsumer wires the production dependencies: the discovery snapshot
// resolves repositories, RefreshOnce executes them.
func newRefreshConsumer(source refreshSource, kv refreshKV, snap *snapshot, deps RepoDeps, logger *slog.Logger) *refreshConsumer {
	return &refreshConsumer{
		source: source,
		kv:     kv,
		resolve: func(id string) (provider.Repo, error) {
			repos := snap.get()
			if len(repos) == 0 {
				return provider.Repo{}, fmt.Errorf("refresh: %w: discovery snapshot is empty", errRefreshTransient)
			}
			return resolveRepo(repos, id)
		},
		run:    func(ctx context.Context, repo provider.Repo) RefreshResult { return RefreshOnce(ctx, deps, repo) },
		logger: logger,
	}
}

// consume processes messages until ctx is canceled (T112).
func (c *refreshConsumer) consume(ctx context.Context) error {
	for {
		msgs, err := c.source.Fetch(ctx, 1, time.Second)
		switch {
		case ctx.Err() != nil:
			c.logStats()
			return nil
		case errors.Is(err, nats.ErrTimeout):
			continue
		case err != nil:
			c.logger.Error("refresh: fetch failed", "err", err)
			select {
			case <-ctx.Done():
				c.logStats()
				return nil
			case <-time.After(time.Second):
			}
			continue
		}
		for _, msg := range msgs {
			c.handle(ctx, msg)
		}
	}
}

// handle processes one task (T112): coalesce guard → in_progress → work →
// done/failed → ack. Transient failures are Nak-ed (retry with backoff); after
// MaxDeliver the task is Term-ed with the KV status kept as failed.
func (c *refreshConsumer) handle(ctx context.Context, msg refreshMsg) {
	c.stats.received.Add(1)
	var task refreshTask
	if err := json.Unmarshal(msg.Data(), &task); err != nil {
		c.logger.Error("refresh: bad task", "err", err)
		_ = msg.Term()
		c.stats.failed.Add(1)
		return
	}
	key := repoSlug(task.Repo)

	// coalesce guard: a duplicate request for a fresh run/result is a no-op.
	// The in_progress window equals the effective ack wait, so a redelivery
	// after a crash is older than the window and proceeds (T112).
	if st, ok := c.readStatus(key); ok {
		now := time.Now()
		fresh := (st.State == "in_progress" && now.Sub(st.StartedAt) < refreshCoalesceWindow) ||
			(st.State == "done" && now.Sub(st.FinishedAt) < refreshDoneFresh)
		if fresh {
			c.stats.coalesced.Add(1)
			c.logger.Info("refresh coalesced", "repo", task.Repo, "state", st.State)
			_ = msg.Ack()
			return
		}
	}

	repo, err := c.resolve(task.Repo)
	if err != nil {
		if errors.Is(err, errRefreshTransient) {
			_ = msg.Nak() // the discovery listing has not produced data yet
			return
		}
		// no retry can fix an unknown/ambiguous repository — terminal.
		c.writeStatus(key, refreshStatus{State: "failed", RequestID: task.RequestID,
			FinishedAt: time.Now().UTC(), Errors: []string{err.Error()}})
		_ = msg.Term()
		c.stats.failed.Add(1)
		c.logger.Error("refresh failed", "repo", task.Repo, "err", err)
		return
	}

	started := time.Now().UTC()
	if err := c.writeStatus(key, refreshStatus{State: "in_progress", RequestID: task.RequestID, StartedAt: started}); err != nil {
		c.logger.Error("refresh: KV in_progress failed", "repo", task.Repo, "err", err)
		_ = msg.Nak() // transient — retry
		return
	}

	res := c.run(ctx, repo)
	status := refreshStatus{
		State: "done", RequestID: task.RequestID, StartedAt: started,
		FinishedAt: time.Now().UTC(), CommitsIndexed: res.CommitsIndexed,
		MetaUpdated: res.MetaUpdated, Errors: res.Errors, DurationMS: res.Duration.Milliseconds(),
	}
	if len(res.Errors) > 0 {
		status.State = "failed" // partial result; errors[] carries the detail
	}
	// ack strictly after the Qdrant upserts, checkpoints and this KV write.
	if err := c.writeStatus(key, status); err != nil {
		c.logger.Error("refresh: KV status failed", "repo", task.Repo, "err", err)
		_ = msg.Nak() // transient — retry
		return
	}
	switch {
	case status.State == "done":
		_ = msg.Ack()
		c.stats.ok.Add(1)
		c.logger.Info("refresh done", "repo", task.Repo,
			"commits", res.CommitsIndexed, "meta", res.MetaUpdated,
			"duration_ms", res.Duration.Milliseconds())
	case msg.NumDelivered() >= refreshMaxDeliver:
		_ = msg.Term() // retries exhausted — terminal (KV failed is written)
		c.stats.failed.Add(1)
		c.logger.Error("refresh failed (terminal)", "repo", task.Repo,
			"delivery", msg.NumDelivered(), "errors", res.Errors)
	default:
		_ = msg.Nak() // transient/partial — retry with the consumer backoff
		c.stats.failed.Add(1)
		c.logger.Warn("refresh failed (retry)", "repo", task.Repo,
			"delivery", msg.NumDelivered(), "errors", res.Errors)
	}
}

// readStatus reads the KV status; a missing key (or an unreadable value) is "no status".
func (c *refreshConsumer) readStatus(key string) (refreshStatus, bool) {
	data, err := c.kv.Get(key)
	if err != nil {
		return refreshStatus{}, false
	}
	var st refreshStatus
	if err := json.Unmarshal(data, &st); err != nil {
		return refreshStatus{}, false
	}
	return st, true
}

// writeStatus marshals and writes the KV status.
func (c *refreshConsumer) writeStatus(key string, st refreshStatus) error {
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return c.kv.Put(key, data)
}

// logStats writes the lane counters (T112).
func (c *refreshConsumer) logStats() {
	c.logger.Info("refresh consumer stopped",
		"received", c.stats.received.Load(), "coalesced", c.stats.coalesced.Load(),
		"ok", c.stats.ok.Load(), "failed", c.stats.failed.Load())
}

// runRefreshLane starts the refresh consumer when NATS_URL is set (T108/T112).
// A NATS outage does not kill the background lanes: the connection is retried
// with a pause (FR-014).
func runRefreshLane(ctx context.Context, cfg *config.Config, deps RepoDeps, snap *snapshot, logger *slog.Logger) error {
	names := config.RefreshNames(cfg.NATS.InstanceID)
	for {
		err := refreshLaneOnce(ctx, cfg, names, deps, snap, logger)
		if ctx.Err() != nil {
			return nil
		}
		logger.Error("refresh lane failed, retrying", "err", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(refreshLaneBackoff):
		}
	}
}

// refreshLaneOnce connects, provisions the instance topology and runs the consumer.
func refreshLaneOnce(ctx context.Context, cfg *config.Config, names config.RefreshTopology, deps RepoDeps, snap *snapshot, logger *slog.Logger) error {
	nc, err := connectNATS(cfg.NATS.URL, names.ConnName, logger)
	if err != nil {
		return err
	}
	defer nc.Close()
	js, err := nc.JetStream()
	if err != nil {
		return fmt.Errorf("nats: jetstream: %w", err)
	}
	if err := ensureRefreshTopology(js, names); err != nil {
		return err
	}
	kv, err := js.KeyValue(names.KVBucket)
	if err != nil {
		return fmt.Errorf("nats: KV %s: %w", names.KVBucket, err)
	}
	sub, err := js.PullSubscribe(names.Subject, names.Consumer)
	if err != nil {
		return fmt.Errorf("nats: pull subscribe: %w", err)
	}
	consumer := newRefreshConsumer(natsSource{sub}, natsKV{kv}, snap, deps, log.WithPhase(logger, "refresh"))
	logger.Info("refresh consumer started",
		"instance", names.InstanceID, "stream", names.Stream, "consumer", names.Consumer, "subject", names.Subject)
	return consumer.consume(ctx)
}
