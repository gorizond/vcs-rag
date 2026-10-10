package provider

import (
	"errors"
	"net/http"
	"strings"
	"time"

	gh "github.com/google/go-github/v92/github"
	gitlab "gitlab.com/gitlab-org/api/client-go/v3"
)

// IsRateLimit распознаёт ошибки лимитов VCS (FR-015) и возвращает задержку
// до повтора (0 — значение неизвестно, паузу выбирает вызывающий).
//
// GitHub: *github.AbuseRateLimitError (RetryAfter) и *github.RateLimitError
// (Rate.Reset). GitLab: client-go/v3 имеет собственный клиентский RateLimiter
// (WithCustomLimiter) и при 429 возвращает *gitlab.ErrorResponse.
// Generic: «429»/«Retry-After» в тексте ошибки.
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
