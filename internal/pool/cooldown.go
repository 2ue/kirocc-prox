package pool

import "time"

// DefaultBaseBackoff is the first cooldown duration after a rate-limit hit.
const DefaultBaseBackoff = 30 * time.Second

// DefaultMaxBackoff caps the exponential backoff at this duration.
const DefaultMaxBackoff = 30 * time.Minute

// NextBackoff returns the duration to wait before re-selecting a credential
// that just hit a rate limit. The schedule is 30s, 60s, 120s, ..., capped at
// 30 minutes (DefaultMaxBackoff). If retryAfter is non-zero it overrides the
// exponential schedule (clamped to [DefaultBaseBackoff, DefaultMaxBackoff]).
func NextBackoff(level int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		if retryAfter < DefaultBaseBackoff {
			return DefaultBaseBackoff
		}
		if retryAfter > DefaultMaxBackoff {
			return DefaultMaxBackoff
		}
		return retryAfter
	}
	if level < 0 {
		level = 0
	}
	// Avoid shift overflow; cap level so the result never exceeds maxBackoff.
	if level > 6 {
		level = 6
	}
	d := DefaultBaseBackoff << level
	if d > DefaultMaxBackoff {
		return DefaultMaxBackoff
	}
	return d
}
