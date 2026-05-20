// [fork] New file added in fork. Small helpers used by the messages handler
// to derive the "device" and "credits-used snapshot" fields for the admin
// history panel.

package messages

import (
	"strings"

	"github.com/niuma/kirocc-pro/internal/pool"
)

// summarizeUserAgent reduces a User-Agent header to its first whitespace-
// separated token, e.g.
//
//	"claude-code/1.0.0 (Macintosh; ...)" -> "claude-code/1.0.0"
//	"curl/8.4.0"                         -> "curl/8.4.0"
//
// Empty input returns "" (the admin UI renders this as "—").
func summarizeUserAgent(ua string) string {
	ua = strings.TrimSpace(ua)
	if ua == "" {
		return ""
	}
	if i := strings.IndexByte(ua, ' '); i > 0 {
		ua = ua[:i]
	}
	if len(ua) > 64 {
		ua = ua[:64]
	}
	return ua
}

// credCreditsSnapshot reads cred.LastQuota.CreditsUsed and CreditsTotal
// under the credential's read lock. Returns 0/0 when the quota poller has
// not populated a snapshot yet (the UI surfaces this as "—" instead of a
// misleading 0).
func credCreditsSnapshot(cred *pool.Credential) (used, total float64) {
	if cred == nil {
		return 0, 0
	}
	cred.Mu.RLock()
	defer cred.Mu.RUnlock()
	if cred.LastQuota == nil {
		return 0, 0
	}
	return cred.LastQuota.CreditsUsed, cred.LastQuota.CreditsTotal
}
