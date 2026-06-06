// [fork] New file added in fork. Bridges request-completion signals to the
// pool scheduler (MarkSuccess / MarkRateLimit / MarkAuthError) and to the
// usage aggregator, so the admin dashboard and credential cooldown logic
// see every request outcome.
package messages

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/niuma/kirocc-pro/internal/kiroclient"
	"github.com/niuma/kirocc-pro/internal/pool"
	"github.com/niuma/kirocc-pro/internal/usage"
)

// markPoolOutcome reports the request result to the scheduler. The
// classification prefers structured upstream errors and falls back to string
// fragments for locally-created errors.
func (s *Service) markPoolOutcome(credID, model string, err error, mw *metricsResponseWriter, latency time.Duration) {
	if s.scheduler == nil || credID == "" {
		return
	}
	if err == nil {
		s.scheduler.MarkSuccess(credID, model, pool.Usage{
			InputTokens:      mw.inputTokens,
			OutputTokens:     mw.outputTokens,
			CacheReadTokens:  mw.cacheReadTokens,
			CacheWriteTokens: mw.cacheWriteTokens,
			LatencyMs:        int(latency.Milliseconds()),
		})
		return
	}

	var ue *kiroclient.UpstreamError
	if errors.As(err, &ue) {
		switch {
		case isRateLimitUpstreamError(ue):
			s.scheduler.MarkRateLimit(credID, model, ue.RetryAfter)
		case isAuthUpstreamError(ue):
			s.scheduler.MarkAuthError(credID, ue.Error())
		}
		return
	}

	msg := err.Error()
	switch classifyErrorText(msg) {
	case usage.StatusRateLimited:
		s.scheduler.MarkRateLimit(credID, model, 0)
	case usage.StatusAuthError:
		s.scheduler.MarkAuthError(credID, msg)
	default:
		// Unknown error class; record nothing so it doesn't bias the
		// backoff exponent. The dashboard still sees it via the usage
		// aggregator.
	}
}

// usageStatusFor maps an error to a usage.Status* constant.
func usageStatusFor(err error) string {
	if err == nil {
		return usage.StatusSuccess
	}

	var ue *kiroclient.UpstreamError
	if errors.As(err, &ue) {
		switch {
		case isRateLimitUpstreamError(ue):
			return usage.StatusRateLimited
		case isAuthUpstreamError(ue):
			return usage.StatusAuthError
		default:
			return usage.StatusUpstreamError
		}
	}
	status := classifyErrorText(err.Error())
	if status != "" {
		return status
	}
	return usage.StatusUpstreamError
}

func isRateLimitUpstreamError(ue *kiroclient.UpstreamError) bool {
	if ue == nil {
		return false
	}
	return ue.Status == http.StatusTooManyRequests || isThrottleException(ue.Exception)
}

func isAuthUpstreamError(ue *kiroclient.UpstreamError) bool {
	if ue == nil {
		return false
	}
	return ue.Status == http.StatusForbidden || ue.Status == http.StatusUnauthorized ||
		isAuthException(ue.Exception) || classifyErrorText(ue.Body) == usage.StatusAuthError
}

func isThrottleException(exception string) bool {
	switch exception {
	case "ThrottlingException", "TooManyRequestsException", "RequestLimitExceeded", "LimitExceededException":
		return true
	default:
		return false
	}
}

func isAuthException(exception string) bool {
	switch exception {
	case "AccessDeniedException", "UnauthorizedException", "UnrecognizedClientException",
		"ExpiredTokenException", "InvalidSignatureException":
		return true
	default:
		return false
	}
}

func classifyErrorText(msg string) string {
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "429"),
		strings.Contains(lower, "rate limit"),
		strings.Contains(lower, "rate_limited"),
		strings.Contains(lower, "too many requests"),
		strings.Contains(lower, "throttl"):
		return usage.StatusRateLimited
	case strings.Contains(lower, "403"),
		strings.Contains(lower, "401"),
		strings.Contains(lower, "auth"),
		strings.Contains(lower, "banned"),
		strings.Contains(lower, "unauthorized"),
		strings.Contains(lower, "forbidden"):
		return usage.StatusAuthError
	default:
		return ""
	}
}
