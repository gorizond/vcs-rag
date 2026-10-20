package provider

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	gitlab "gitlab.com/gitlab-org/api/client-go/v3"
)

// T136: a GitLab 429 carries the Retry-After of the HTTP response (seconds or
// an HTTP-date); absent/garbage values fall back to 0 with ok=true.
// Note: client-go/v3 keeps the status on ErrorResponse itself (StatusCode)
// while the Retry-After header lives on the embedded *http.Response.
func TestIsRateLimitGitLabRetryAfter(t *testing.T) {
	glErr := func(h http.Header) error {
		return &gitlab.ErrorResponse{
			StatusCode: http.StatusTooManyRequests,
			Response: &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     h,
				Request:    &http.Request{URL: &url.URL{}},
			},
		}
	}

	if d, ok := IsRateLimit(glErr(http.Header{"Retry-After": {"7"}})); !ok || d != 7*time.Second {
		t.Errorf("seconds: d = %v ok = %v, want 7s/true", d, ok)
	}

	date := time.Now().Add(45 * time.Second).UTC().Format(http.TimeFormat)
	if d, ok := IsRateLimit(glErr(http.Header{"Retry-After": {date}})); !ok || d <= 30*time.Second || d > 45*time.Second {
		t.Errorf("http-date: d = %v ok = %v, want ~45s/true", d, ok)
	}

	if d, ok := IsRateLimit(glErr(nil)); !ok || d != 0 {
		t.Errorf("no header: d = %v ok = %v, want 0/true", d, ok)
	}
	if d, ok := IsRateLimit(glErr(http.Header{"Retry-After": {"garbage"}})); !ok || d != 0 {
		t.Errorf("garbage: d = %v ok = %v, want 0/true", d, ok)
	}
	if d, ok := IsRateLimit(glErr(http.Header{"Retry-After": {"-5"}})); !ok || d != 0 {
		t.Errorf("negative: d = %v ok = %v, want 0/true", d, ok)
	}

	// non-429 GitLab responses are not rate limits.
	other := &gitlab.ErrorResponse{
		StatusCode: http.StatusUnauthorized,
		Response: &http.Response{
			StatusCode: http.StatusUnauthorized,
			Request:    &http.Request{URL: &url.URL{}},
		},
	}
	if _, ok := IsRateLimit(other); ok {
		t.Error("401 must not be a rate limit")
	}
	if _, ok := IsRateLimit(&gitlab.ErrorResponse{}); ok {
		t.Error("a status-less ErrorResponse is not a 429")
	}
}

// T136: git-fetch/transport errors classify via the string fallback.
func TestIsRateLimitGitFetch429(t *testing.T) {
	git429 := fmt.Errorf("git remote update --prune: exit status 128: fatal: unable to access 'https://git.example/repo.git/': The requested URL returned error: 429")
	if _, ok := IsRateLimit(git429); !ok {
		t.Error("a git-fetch 429 must classify as a rate limit")
	}
	if _, ok := IsRateLimit(errors.New("HTTP 429 Too Many Requests")); !ok {
		t.Error("a generic 429 must classify")
	}
	if _, ok := IsRateLimit(errors.New("fatal: repository 'https://git.example/x.git' not found")); ok {
		t.Error("a 404-style git error must not classify")
	}
	if _, ok := IsRateLimit(nil); ok {
		t.Error("nil is not a rate limit")
	}
}

// T009 (US1): the three classification cases of the spec — GitLab 429 without
// Retry-After (fallback floor), GitLab 429 with Retry-After: 7, git-fetch 429.
func TestIsRateLimitUS1Cases(t *testing.T) {
	glErr := func(h http.Header) error {
		return &gitlab.ErrorResponse{
			StatusCode: http.StatusTooManyRequests,
			Response:   &http.Response{StatusCode: http.StatusTooManyRequests, Header: h},
		}
	}
	cases := []struct {
		name string
		err  error
		want time.Duration
	}{
		{"gitlab 429 without Retry-After", glErr(nil), 0},
		{"gitlab 429 with Retry-After: 7", glErr(http.Header{"Retry-After": {"7"}}), 7 * time.Second},
		{"git fetch 429 from stderr", errors.New("git remote update --prune: exit status 128: fatal: unable to access 'https://git.example/x.git/': The requested URL returned error: 429"), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, ok := IsRateLimit(tc.err)
			if !ok || d != tc.want {
				t.Errorf("IsRateLimit = %v/%v, want %v/true", d, ok, tc.want)
			}
		})
	}
}

// T015 (US1): access errors (403/404) are classified separately from rate
// limits, typed or as git transport text.
func TestIsAccessError(t *testing.T) {
	glErr := func(code int) error {
		return &gitlab.ErrorResponse{StatusCode: code, Response: &http.Response{StatusCode: code}}
	}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"gitlab 403", glErr(http.StatusForbidden), true},
		{"gitlab 404", glErr(http.StatusNotFound), true},
		{"gitlab 401", glErr(http.StatusUnauthorized), false},
		{"git fetch 403", errors.New("The requested URL returned error: 403"), true},
		{"git fetch 404", errors.New("The requested URL returned error: 404"), true},
		{"generic 429", errors.New("HTTP 429 Too Many Requests"), false},
		{"plain failure", errors.New("connection refused"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsAccessError(tc.err); got != tc.want {
				t.Errorf("IsAccessError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
