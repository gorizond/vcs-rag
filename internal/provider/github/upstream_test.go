package github

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorizond/vcs-rag/internal/config"
	"github.com/gorizond/vcs-rag/internal/provider"
)

// roundTripFunc is an inline http.RoundTripper for transport tests.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testResponse(status int, header http.Header) *http.Response {
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader("{}"))}
}

func testRequest(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.github.com/rate_limit", nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// T062: every API request goes through the shared gate; the peak concurrency
// equals WORKER_UPSTREAM_CONCURRENCY.
func TestTransportGateLimitsConcurrency(t *testing.T) {
	var active, peak atomic.Int64
	base := roundTripFunc(func(*http.Request) (*http.Response, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
		return testResponse(http.StatusOK, http.Header{}), nil
	})
	gate := provider.NewUpstreamGate(1)
	tr := &upstreamTransport{base: base}
	tr.gate.Store(gate)
	tr.queue.Store(provider.NewUpstreamQueue(gate, 16))

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.github.com/rate_limit", nil)
			resp, err := tr.RoundTrip(req)
			if err != nil {
				t.Errorf("RoundTrip: %v", err)
				return
			}
			resp.Body.Close()
		}()
	}
	wg.Wait()
	if got := peak.Load(); got != 1 {
		t.Errorf("peak concurrent API requests = %d, want 1", got)
	}
}

// T062/FR-008: GitHub's primary 403 (exhausted X-RateLimit-Remaining) and the
// secondary 429 (Retry-After) both count as rate-limit events; the last
// Retry-After is captured.
func TestTransportCountsRateLimitAnswers(t *testing.T) {
	calls := 0
	base := roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		h := http.Header{}
		switch calls {
		case 1:
			h.Set("X-RateLimit-Limit", "5000")
			h.Set("X-RateLimit-Remaining", "0")
			h.Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10))
			return testResponse(http.StatusForbidden, h), nil
		case 2:
			h.Set("Retry-After", "7")
			return testResponse(http.StatusTooManyRequests, h), nil
		default:
			return testResponse(http.StatusOK, http.Header{}), nil
		}
	})
	tr := &upstreamTransport{base: base}
	tr.gate.Store(provider.NewUpstreamGate(1))

	for i := 0; i < 2; i++ {
		resp, err := tr.RoundTrip(testRequest(t))
		if err != nil {
			t.Fatalf("RoundTrip %d: %v", i+1, err)
		}
		resp.Body.Close()
	}
	if got := tr.rateEvents.Load(); got != 2 {
		t.Errorf("rate-limit events = %d, want 2 (403 primary + 429 secondary)", got)
	}
	if got := time.Duration(tr.retry.Load()); got != 7*time.Second {
		t.Errorf("last Retry-After = %v, want 7s", got)
	}
	if used, ok := tr.usage(); !ok || used != 1 {
		t.Errorf("usage = %v/%v, want 1/true from X-RateLimit-*", used, ok)
	}
}

// T062/R3: when X-RateLimit-Remaining drops below 10% of the limit, the
// client parks until X-RateLimit-Reset instead of issuing the next request.
func TestTransportProactiveCooldown(t *testing.T) {
	first := true
	base := roundTripFunc(func(*http.Request) (*http.Response, error) {
		if first {
			first = false
			h := http.Header{}
			h.Set("X-RateLimit-Limit", "5000")
			h.Set("X-RateLimit-Remaining", "100") // 2%
			h.Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
			return testResponse(http.StatusOK, h), nil
		}
		return testResponse(http.StatusOK, http.Header{}), nil
	})
	tr := &upstreamTransport{base: base}
	tr.gate.Store(provider.NewUpstreamGate(1))

	resp, err := tr.RoundTrip(testRequest(t))
	if err != nil {
		t.Fatalf("first request: %v", err)
	}
	resp.Body.Close()
	if used, ok := tr.usage(); !ok || used < 0.97 {
		t.Fatalf("usage = %v/%v, want ~0.98", used, ok)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/rate_limit", nil)
	if _, err := tr.RoundTrip(req); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("second request err = %v, want the cooldown wait to end via ctx", err)
	}
}

// T062: New wires the wrapped transport; SetUpstreamGate/SetUpstreamQueue and
// SetPageCharger are honoured by a real client request.
func TestClientWiringAndPageCharger(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	cfg := &config.Config{}
	cfg.Provider.GitHubToken = "test-token"
	cfg.Provider.GitHubOrgs = []string{"acme"}
	cfg.Provider.GitHubBaseURL = srv.URL
	cfg.Worker.CIWindow = 1
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gate := provider.NewUpstreamGate(1)
	queue := provider.NewUpstreamQueue(gate, 10)
	c.SetUpstreamGate(gate)
	c.SetUpstreamQueue(queue)
	var charged int
	c.SetPageCharger(func() { charged++ })

	if _, err := c.ListRepositories(context.Background()); err != nil {
		t.Fatalf("ListRepositories: %v", err)
	}
	if c.Pages() != 1 {
		t.Errorf("pages = %d, want 1", c.Pages())
	}
	if charged != 1 {
		t.Errorf("page charger calls = %d, want 1", charged)
	}
}
