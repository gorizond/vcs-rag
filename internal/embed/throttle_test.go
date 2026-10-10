package embed

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// throttleServer replies to every request with the given status/body after delay.
func throttleServer(t *testing.T, status int, body string, delay time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// T127: overload statuses (400 "No models loaded", 429, 5xx) are throttle;
// auth/not-found statuses are not.
func TestIsThrottleHTTPStatuses(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"400 no models loaded", http.StatusBadRequest, `{"error":"No models loaded"}`, true},
		{"400 case-insensitive", http.StatusBadRequest, `NO MODELS LOADED`, true},
		{"400 other", http.StatusBadRequest, `{"error":"bad input"}`, false},
		{"429", http.StatusTooManyRequests, `rate limited`, true},
		{"500", http.StatusInternalServerError, `boom`, true},
		{"503", http.StatusServiceUnavailable, `unavailable`, true},
		{"401", http.StatusUnauthorized, `unauthorized`, false},
		{"404", http.StatusNotFound, `model not found`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := throttleServer(t, tc.status, tc.body, 0)
			p := NewOllama(srv.URL, "m", 2, 8)
			_, err := p.Embed(context.Background(), []string{"a"})
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := IsThrottle(err); got != tc.want {
				t.Errorf("IsThrottle(%v) = %v, want %v", err, got, tc.want)
			}
		})
	}
}

// T127: the LM Studio client classifies the same way.
func TestIsThrottleLMStudio(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   bool
	}{
		{http.StatusTooManyRequests, true},
		{http.StatusNotFound, false},
	} {
		srv := throttleServer(t, tc.status, `nope`, 0)
		p := NewLMStudio(srv.URL, "m", 2, 8)
		_, err := p.Embed(context.Background(), []string{"a"})
		if err == nil || IsThrottle(err) != tc.want {
			t.Errorf("status %d: err = %v, IsThrottle = %v, want %v", tc.status, err, IsThrottle(err), tc.want)
		}
	}
}

// T127: transport errors — timeout/refused are throttle, caller cancel is not.
func TestIsThrottleTransport(t *testing.T) {
	slow := throttleServer(t, http.StatusOK, `{"embeddings":[[1,2]]}`, 300*time.Millisecond)
	p := NewOllama(slow.URL, "m", 2, 8)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := p.Embed(ctx, []string{"a"})
	if err == nil || !IsThrottle(err) {
		t.Errorf("timeout: err = %v, IsThrottle = %v, want true", err, IsThrottle(err))
	}

	canceled, cancelFn := context.WithCancel(context.Background())
	cancelFn()
	_, err = p.Embed(canceled, []string{"a"})
	if err == nil || IsThrottle(err) {
		t.Errorf("canceled: err = %v, IsThrottle = %v, want false", err, IsThrottle(err))
	}

	// a closed local port → connection refused.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	refused := NewOllama("http://"+addr, "m", 2, 8)
	_, err = refused.Embed(context.Background(), []string{"a"})
	if err == nil || !IsThrottle(err) {
		t.Errorf("refused: err = %v, IsThrottle = %v, want true", err, IsThrottle(err))
	}
}

// T127: dimension mismatch, parse errors and plain errors are not throttle.
func TestIsThrottleNonThrottle(t *testing.T) {
	if IsThrottle(nil) {
		t.Error("nil is not a throttle")
	}
	if IsThrottle(fmt.Errorf("plain")) {
		t.Error("a plain error is not a throttle")
	}

	// dimension mismatch: dim=2, the server returns dim=3.
	srv := throttleServer(t, http.StatusOK, `{"embeddings":[[1,2,3]]}`, 0)
	p := NewOllama(srv.URL, "m", 2, 8)
	_, err := p.Embed(context.Background(), []string{"a"})
	if err == nil || IsThrottle(err) {
		t.Errorf("dim mismatch: err = %v, IsThrottle = %v, want false", err, IsThrottle(err))
	}

	// parse error: 200 with invalid JSON.
	bad := throttleServer(t, http.StatusOK, `{not json`, 0)
	p2 := NewOllama(bad.URL, "m", 2, 8)
	_, err = p2.Embed(context.Background(), []string{"a"})
	if err == nil || IsThrottle(err) {
		t.Errorf("parse: err = %v, IsThrottle = %v, want false", err, IsThrottle(err))
	}

	// a wrapped ThrottleError is recognized through the chain.
	wrapped := fmt.Errorf("lane: %w", &ThrottleError{Provider: "ollama", Reason: "timeout"})
	if !IsThrottle(wrapped) {
		t.Error("a wrapped throttle must be recognized")
	}
}
