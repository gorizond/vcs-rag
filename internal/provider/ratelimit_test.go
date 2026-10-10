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
