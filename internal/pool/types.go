// Package pool implements a credential pool with multi-account scheduling,
// per-credential cooldown after rate limits, and session affinity. When the
// pool holds a single credential it degrades to direct passthrough, matching
// the upstream d-kuro/kirocc behavior.
package pool

import (
	"sync"
	"time"

	"github.com/niuma/kirocc-pro/internal/auth"
)

// Credential is one Kiro account in the pool.
//
// The embedded auth.Credentials carries OAuth tokens and is refreshed in
// place by the auth package. Fields above Mu are immutable after load (or
// only touched by the auth refresh path with the credential locked).
//
// Locking discipline: callers must acquire Mu (write lock for mutation,
// read lock for snapshot reads) when accessing any field at or below Mu.
// Helper methods on *Credential lock internally; raw field access requires
// manual locking.
type Credential struct {
	// ===== immutable identity (set at load time) =====
	ID             string // stable across restarts (e.g. "kiro-alice-001")
	Label          string // UI display name; may be masked
	Provider       string // upstream provider id (e.g. "kiro", "codex", "gemini"); "" = "kiro" for backwards-compat
	Priority       int    // higher wins ties; default 100
	Disabled       bool   // operator override; never selected
	DisableCooling bool   // skip cooldown after rate limits (unlimited-tier keys)
	ProxyURL       string // optional outbound proxy for this account's auth-plane HTTP (token refresh / quota / OAuth)

	// ===== OAuth tokens — refreshable; lifetime managed by auth package =====
	auth.Credentials

	// ===== runtime state =====
	Mu          sync.RWMutex
	Quota       QuotaState
	ModelStates map[string]*ModelState
	Success     int64
	Failed      int64
	LastUsedAt  time.Time

	DisabledReason string    // populated when Disabled is set due to error
	DisabledAt     time.Time // zero when Disabled was set manually

	// Metadata is a provider-specific bag for fields that don't fit the
	// generic Credentials struct. Examples: Codex stores
	// chatgpt_account_id here so the executor can set the required
	// "Chatgpt-Account-Id" header. Read under Mu.
	Metadata map[string]string

	// Last successful Kiro getUsageLimits response (or nil if never fetched).
	LastQuota        *KiroQuotaSnapshot
	LastQuotaAt      time.Time
	LastQuotaError   string
	LastQuotaErrorAt time.Time
}

// QuotaState tracks rate-limit / quota state at either account or model level.
type QuotaState struct {
	Exceeded      bool      // currently rate-limited
	NextRecoverAt time.Time // skip selection until this time
	BackoffLevel  int       // exponential backoff exponent; 0 = no prior cooldown
}

// ModelState is per-model runtime state on a credential.
type ModelState struct {
	Quota   QuotaState
	Success int64
	Failed  int64
}

// KiroQuotaSnapshot is the parsed shape of a Kiro getUsageLimits response.
// Field names mirror the JSON keys in the response; nullable fields are
// pointers so absence is distinguishable from zero.
type KiroQuotaSnapshot struct {
	FetchedAt       time.Time
	PlanName        string  // from subscriptionInfo.subscriptionTitle
	PlanTier        string  // from subscriptionInfo.type
	CreditsTotal    float64 // usageLimitWithPrecision
	CreditsUsed     float64 // currentUsageWithPrecision
	BonusTotal      float64 // freeTrialInfo.usageLimitWithPrecision
	BonusUsed       float64 // freeTrialInfo.currentUsageWithPrecision
	BonusExpireDays int     // freeTrialInfo.daysRemaining
	NextResetAt     time.Time
	Banned          bool   // 403 + body contains "BANNED:" prefix
	BanReason       string // captured from body when Banned
}

// Usage is the per-request token accounting reported by handlers after each
// upstream call. It is published to the usage subsystem AND used to update
// the credential's Success/Failed counters via Scheduler.MarkSuccess.
type Usage struct {
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	LatencyMs        int
}

// View is an immutable snapshot of a Credential, safe to expose via the
// admin API. It does NOT include the OAuth tokens.
type View struct {
	ID             string
	Label          string
	Provider       string
	Priority       int
	Disabled       bool
	DisableCooling bool
	DisabledReason string
	DisabledAt     time.Time

	Region     string
	ProfileARN string
	AuthType   string
	ProxyURL   string

	Quota       QuotaStateView
	ModelStates map[string]ModelStateView
	Success     int64
	Failed      int64
	LastUsedAt  time.Time

	LastQuota        *KiroQuotaSnapshot
	LastQuotaAt      time.Time
	LastQuotaError   string
	LastQuotaErrorAt time.Time
}

// QuotaStateView is a copy of QuotaState safe to expose externally.
type QuotaStateView struct {
	Exceeded      bool
	NextRecoverAt time.Time
	BackoffLevel  int
}

// ModelStateView is a copy of ModelState safe to expose externally.
type ModelStateView struct {
	Quota   QuotaStateView
	Success int64
	Failed  int64
}

// IsReady reports whether the credential can be selected right now.
// Acquires Mu (read lock).
func (c *Credential) IsReady() bool {
	c.Mu.RLock()
	defer c.Mu.RUnlock()
	if c.Disabled {
		return false
	}
	if c.Quota.Exceeded && time.Now().Before(c.Quota.NextRecoverAt) {
		return false
	}
	return true
}

// IsInCooldown reports whether the credential is currently rate-limited.
// Acquires Mu (read lock).
func (c *Credential) IsInCooldown() bool {
	c.Mu.RLock()
	defer c.Mu.RUnlock()
	return c.Quota.Exceeded && time.Now().Before(c.Quota.NextRecoverAt)
}

// Snapshot returns an immutable view of the credential. Acquires Mu (read lock).
func (c *Credential) Snapshot() View {
	c.Mu.RLock()
	defer c.Mu.RUnlock()
	v := View{
		ID:               c.ID,
		Label:            c.Label,
		Provider:         c.Provider,
		Priority:         c.Priority,
		Disabled:         c.Disabled,
		DisableCooling:   c.DisableCooling,
		DisabledReason:   c.DisabledReason,
		DisabledAt:       c.DisabledAt,
		Region:           c.Region,
		ProfileARN:       c.ProfileARN,
		AuthType:         c.AuthType,
		ProxyURL:         c.ProxyURL,
		Quota:            QuotaStateView(c.Quota),
		Success:          c.Success,
		Failed:           c.Failed,
		LastUsedAt:       c.LastUsedAt,
		LastQuota:        c.LastQuota,
		LastQuotaAt:      c.LastQuotaAt,
		LastQuotaError:   c.LastQuotaError,
		LastQuotaErrorAt: c.LastQuotaErrorAt,
	}
	if len(c.ModelStates) > 0 {
		v.ModelStates = make(map[string]ModelStateView, len(c.ModelStates))
		for k, ms := range c.ModelStates {
			v.ModelStates[k] = ModelStateView{
				Quota:   QuotaStateView(ms.Quota),
				Success: ms.Success,
				Failed:  ms.Failed,
			}
		}
	}
	return v
}
