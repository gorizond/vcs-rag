package provider

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v92/github"
	gitlab "gitlab.com/gitlab-org/api/client-go/v3"
)

// IsRateLimit recognizes VCS rate limit errors (FR-015) and returns the
// delay before retry (0 — value unknown, the caller picks the pause).
//
// GitHub: *github.AbuseRateLimitError (RetryAfter) and *github.RateLimitError
// (Rate.Reset). GitLab: client-go/v3 has its own client-side RateLimiter
// (WithCustomLimiter) and returns *gitlab.ErrorResponse on 429.
// Generic: "429"/"Retry-After" in the error text.
func IsRateLimit(err error) (time.Duration, bool) {
	if err == nil {
		return 0, false
	}
	var abuse *gh.AbuseRateLimitError
	if errors.As(err, &abuse) {
		if abuse.RetryAfter != nil {
			return *abuse.RetryAfter, true
		}
		return 0, true
	}
	var rate *gh.RateLimitError
	if errors.As(err, &rate) {
		if d := time.Until(rate.Rate.Reset.Time); d > 0 {
			return d, true
		}
		return 0, true
	}
	var glErr *gitlab.ErrorResponse
	if errors.As(err, &glErr) && glErr.HasStatusCode(http.StatusTooManyRequests) {
		return retryAfterFromHeader(glErr.Response), true
	}
	// git-fetch/transport errors carry no typed response: the raw text fallback
	// classifies them ("The requested URL returned error: 429", "Retry-After").
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "429") || strings.Contains(msg, "retry-after") || strings.Contains(msg, "too many requests") {
		return 0, true
	}
	return 0, false
}

// retryAfterFromHeader parses the Retry-After header of a GitLab error response:
// seconds ("7") or an HTTP-date; invalid/absent → 0 (the caller picks the pause).
func retryAfterFromHeader(resp *http.Response) time.Duration {
	if resp == nil {
		return 0
	}
	v := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if at, err := http.ParseTime(v); err == nil {
		if d := time.Until(at); d > 0 {
			return d
		}
	}
	return 0
}
