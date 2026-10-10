package provider

import (
	"errors"
	"net/http"
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
		return 0, true
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "429") || strings.Contains(msg, "retry-after") {
		return 0, true
	}
	return 0, false
}
