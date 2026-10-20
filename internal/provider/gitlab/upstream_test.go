package gitlab

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// T028/R3: the client parks until RateLimit-Reset only when Remaining < 10% of
// Limit; absent headers leave the 429-driven behavior unchanged.
func TestObserveRateLimitHeaders(t *testing.T) {
	tr := &upstreamTransport{base: http.DefaultTransport}
	tr.gate.Store(provider.NewUpstreamGate(1))
	reset := time.Now().Add(time.Hour).Truncate(time.Second)
	h := http.Header{}
	h.Set("RateLimit-Limit", "5000")
	h.Set("RateLimit-Remaining", "4999")
	h.Set("RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))

	tr.observe(h)
	if used, ok := tr.usage(); !ok || used >= 0.01 {
		t.Errorf("usage = %v/%v, want ~0 from the observed headers", used, ok)
	}
	tr.mu.Lock()
	until := tr.until
	tr.mu.Unlock()
	if !until.IsZero() {
		t.Fatalf("cooldown started with 99%% of the budget remaining: %v", until)
	}

	h.Set("RateLimit-Remaining", "100") // 2% < 10%
	tr.observe(h)
	tr.mu.Lock()
	until = tr.until
	tr.mu.Unlock()
	if !until.Equal(reset) {
		t.Errorf("cooldown until %v, want RateLimit-Reset %v", until, reset)
	}

	tr2 := &upstreamTransport{}
	tr2.observe(http.Header{})
	tr2.mu.Lock()
	until = tr2.until
	tr2.mu.Unlock()
	if !until.IsZero() {
		t.Error("headers absent must not start a cooldown")
	}
	if _, ok := tr2.usage(); ok {
		t.Error("headers absent must not report a usage")
	}
}

// T022: the narrow-endpoint bucket waits for a refill and honors ctx.
func TestTokenBucketWaitCancels(t *testing.T) {
	b := newTokenBucket(1) // one token per minute
	if err := b.wait(context.Background()); err != nil {
		t.Fatalf("first token: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := b.wait(ctx); err == nil {
		t.Error("an empty bucket must wait, not hand out a token")
	}
}

// T035 (FR-005/FR-016, SC-003): the Wave B transport must not strip or
// overwrite authentication headers — every request, including the first, is
// forwarded with its credentials intact.
func TestTransportPreservesAuthHeaders(t *testing.T) {
	var gotAuth, gotPrivate string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPrivate = r.Header.Get("Private-Token")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	tr := &upstreamTransport{base: http.DefaultTransport}
	tr.gate.Store(provider.NewUpstreamGate(1))
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/api/v4/user", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer canary-oauth")
	req.Header.Set("Private-Token", "canary-private")
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	resp.Body.Close()
	if gotAuth != "Bearer canary-oauth" {
		t.Errorf("Authorization = %q, want it preserved by the wrapper", gotAuth)
	}
	if gotPrivate != "canary-private" {
		t.Errorf("Private-Token = %q, want it preserved by the wrapper", gotPrivate)
	}
}

// T035 (FR-016): the very first API request of a fresh client is already
// authenticated — no anonymous first request (failed-auth ban risk).
func TestFirstRequestIsAuthenticated(t *testing.T) {
	const token = "test-token" // testClient's Provider.GitLabToken
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Private-Token"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)
	if _, err := c.ListGroups(context.Background()); err != nil {
		t.Fatalf("ListGroups: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("no API request was made")
	}
	for i, got := range seen {
		if got != token {
			t.Errorf("request %d: Private-Token = %q, want the configured token on every request", i, got)
		}
	}
}

// blockingRoundTripper holds a request until released (queue-overflow tests).
type blockingRoundTripper struct{ release chan struct{} }

func (b *blockingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	<-b.release
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{},
		Body: io.NopCloser(strings.NewReader("{}"))}, nil
}

// T063 (FR-011): an API request over the shared queue capacity fails fast with
// provider.ErrTooManyTasks instead of waiting indefinitely; admitted work is
// never dropped.
func TestTransportQueueOverflow(t *testing.T) {
	base := &blockingRoundTripper{release: make(chan struct{})}
	gate := provider.NewUpstreamGate(1)
	queue := provider.NewUpstreamQueue(gate, 1) // 1 running + 1 waiting
	tr := &upstreamTransport{base: base}
	tr.gate.Store(gate)
	tr.queue.Store(queue)

	ctx := context.Background()
	var wg sync.WaitGroup
	run := func() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://gitlab.invalid/api/v4/user", nil)
			resp, err := tr.RoundTrip(req)
			if err == nil {
				resp.Body.Close()
			}
		}()
	}
	run() // holds the gate
	for i := 0; i < 200 && queue.Pending() < 1; i++ {
		time.Sleep(time.Millisecond)
	}
	run() // waits in the queue
	for i := 0; i < 200 && queue.Pending() < 2; i++ {
		time.Sleep(time.Millisecond)
	}
	if got := queue.Pending(); got != 2 {
		t.Fatalf("pending = %d, want 2 admitted operations", got)
	}

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://gitlab.invalid/api/v4/user", nil)
	if _, err := tr.RoundTrip(req); !errors.Is(err, provider.ErrTooManyTasks) {
		t.Fatalf("overflow err = %v, want provider.ErrTooManyTasks", err)
	}
	close(base.release)
	wg.Wait()
	if got := queue.Pending(); got != 0 {
		t.Errorf("pending after drain = %d, want 0", got)
	}
}

// T064: the per-pass context charger takes precedence over the client-wide
// fallback — one page response is charged exactly once.
func TestPageChargerContextWins(t *testing.T) {
	ok := func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{},
			Body: io.NopCloser(strings.NewReader("{}"))}, nil
	}
	tr := &upstreamTransport{base: roundTripFunc(ok)}
	tr.gate.Store(provider.NewUpstreamGate(1))
	var installed, contextual int
	tr.setPageCharger(func() { installed++ })

	req := func(ctx context.Context) *http.Request {
		r, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://gitlab.invalid/api/v4/user", nil)
		return r
	}
	ctx := provider.WithPageCharger(context.Background(), func() { contextual++ })
	resp, err := tr.RoundTrip(req(ctx))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if contextual != 1 || installed != 0 {
		t.Fatalf("chargers = context %d / installed %d, want 1/0", contextual, installed)
	}

	resp, err = tr.RoundTrip(req(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if installed != 1 {
		t.Errorf("installed charger = %d, want the fallback to fire once", installed)
	}
}

// roundTripFunc is an inline http.RoundTripper for the charger test.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
