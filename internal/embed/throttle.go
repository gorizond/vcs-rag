package embed

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"syscall"
)

// ThrottleError marks a transient embeddings-backend overload (T127): the
// worker's AIMD limiter reacts to it (halve + cooldown), while permanent errors
// (auth, missing model, dimension mismatch, parse) must not slow the lanes.
type ThrottleError struct {
	Provider string // "ollama" | "lm-studio"
	Status   int    // HTTP status when known (400/429/5xx; 0 for transport errors)
	Reason   string // "no models loaded" | "rate limited" | "server error" | "timeout" | "connection refused"
	Err      error  // wrapped cause when any
}

func (e *ThrottleError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return fmt.Sprintf("%s: %s (HTTP %d)", e.Provider, e.Reason, e.Status)
}

// Unwrap exposes the cause (errors.Is for context.DeadlineExceeded etc.).
func (e *ThrottleError) Unwrap() error { return e.Err }

// IsThrottle reports whether err is a transient backend-overload error.
func IsThrottle(err error) bool {
	var t *ThrottleError
	return errors.As(err, &t)
}

// httpError builds the error of a non-200 embeddings response: overload
// statuses (429, 5xx, 400 "No models loaded") become ThrottleError; auth,
// missing-model and other permanent statuses stay plain errors.
func httpError(provider string, status int, payload []byte) error {
	base := fmt.Errorf("%s: HTTP %d: %s", provider, status, truncate(payload))
	body := strings.ToLower(string(payload))
	switch {
	case status == http.StatusTooManyRequests:
		return &ThrottleError{Provider: provider, Status: status, Reason: "rate limited", Err: base}
	case status >= 500:
		return &ThrottleError{Provider: provider, Status: status, Reason: "server error", Err: base}
	case status == http.StatusBadRequest && strings.Contains(body, "no models loaded"):
		return &ThrottleError{Provider: provider, Status: status, Reason: "no models loaded", Err: base}
	default:
		return base
	}
}

// netError classifies a transport error: timeouts and refused connections mean
// the backend is down/busy (overload); a caller cancellation is not an overload
// signal and must not trigger the limiter's cooldown.
func netError(provider string, err error) error {
	base := fmt.Errorf("%s: %w", provider, err)
	switch {
	case errors.Is(err, context.Canceled):
		return base
	case errors.Is(err, context.DeadlineExceeded):
		return &ThrottleError{Provider: provider, Reason: "timeout", Err: base}
	case errors.Is(err, syscall.ECONNREFUSED):
		return &ThrottleError{Provider: provider, Reason: "connection refused", Err: base}
	}
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return &ThrottleError{Provider: provider, Reason: "timeout", Err: base}
	}
	return base
}
