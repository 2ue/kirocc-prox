// [fork] New file added in fork. Bridges request-completion signals to the
// pool scheduler (MarkSuccess / MarkRateLimit / MarkAuthError) and to the
// usage aggregator, so the admin dashboard and credential cooldown logic
// see every request outcome.
package messages

import (
	"strings"
	"time"

	"github.com/niuma/kirocc-pro/internal/pool"
	"github.com/niuma/kirocc-pro/internal/usage"
)

// markPoolOutcome reports the request result to the scheduler. errMsg is the
// upstream-error message (empty = success). The classification is best-effort:
// substring match against well-known fragments, falling back to MarkSuccess
// so that "retry" outcomes (where the request ultimately succeeded) keep the
// credential's backoff exponent reset.
func (s *Service) markPoolOutcome(credID, model, errMsg string, mw *metricsResponseWriter, latency time.Duration) {
	if s.scheduler == nil || credID == "" {
		return
	}
	if errMsg == "" {
		s.scheduler.MarkSuccess(credID, model, pool.Usage{
			InputTokens:  mw.inputTokens,
			OutputTokens: mw.outputTokens,
			LatencyMs:    int(latency.Milliseconds()),
		})
		return
	}
	lower := strings.ToLower(errMsg)
	switch {
	case strings.Contains(lower, "429"),
		strings.Contains(lower, "rate"),
		strings.Contains(lower, "throttl"),
		strings.Contains(lower, "quota"):
		s.scheduler.MarkRateLimit(credID, model, 0)
	case strings.Contains(lower, "403"),
		strings.Contains(lower, "auth"),
		strings.Contains(lower, "banned"),
		strings.Contains(lower, "forbidden"):
		s.scheduler.MarkAuthError(credID, errMsg)
	default:
		// Unknown error class; record nothing so it doesn't bias the
		// backoff exponent. The dashboard still sees it via the usage
		// aggregator.
	}
}

// usageStatusFor maps an error message to a usage.Status* constant.
func usageStatusFor(errMsg string) string {
	if errMsg == "" {
		return usage.StatusSuccess
	}
	lower := strings.ToLower(errMsg)
	switch {
	case strings.Contains(lower, "429"),
		strings.Contains(lower, "rate"),
		strings.Contains(lower, "throttl"),
		strings.Contains(lower, "quota"):
		return usage.StatusRateLimited
	case strings.Contains(lower, "403"),
		strings.Contains(lower, "auth"),
		strings.Contains(lower, "banned"),
		strings.Contains(lower, "forbidden"):
		return usage.StatusAuthError
	default:
		return usage.StatusUpstreamError
	}
}
