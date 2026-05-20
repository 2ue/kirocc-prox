package pool

import (
	"errors"
	"time"
)

// ErrCredentialNotFound is returned by Scheduler operations on an unknown ID.
var ErrCredentialNotFound = errors.New("pool: credential not found")

// ErrDuplicateID is returned by Scheduler.Add when a credential with the
// same ID is already registered.
var ErrDuplicateID = errors.New("pool: duplicate credential id")

// Scheduler maintains the pool of credentials and their runtime state.
// Implementations must be safe for concurrent use.
//
// Lifecycle: Register at startup, then handlers call Ready / Lookup, and
// after each upstream call exactly one of MarkSuccess / MarkRateLimit /
// MarkAuthError is recorded. The quota poller calls RefreshQuota /
// RecordQuotaError. Admin endpoints call All / Lookup / SetEnabled.
type Scheduler interface {
	// Register replaces the pool with the given credentials. Subsequent
	// Ready / Lookup / All calls reflect the new set. Runtime state on
	// previously-registered credentials with the same ID is preserved
	// across re-registration (so cooldowns survive a creds file reload).
	Register(creds []*Credential)

	// Ready returns a snapshot of currently-selectable credentials,
	// sorted by Priority descending. Excludes Disabled and in-cooldown.
	Ready() []*Credential

	// Lookup returns the credential with the given ID, or nil if absent.
	Lookup(id string) *Credential

	// All returns a snapshot of every credential, including Disabled and
	// in-cooldown. Used by the admin API.
	All() []*Credential

	// MarkSuccess records a successful upstream call. Resets the backoff
	// exponent on both account- and model-level QuotaState (if model is
	// non-empty). Increments Success counters.
	MarkSuccess(credID, model string, u Usage)

	// MarkRateLimit records a 429 / quota error. Schedules cooldown via
	// NextBackoff(level, retryAfter) on both account- and model-level
	// state. If the credential has DisableCooling set, only counters
	// move (no cooldown). Increments Failed counter.
	MarkRateLimit(credID, model string, retryAfter time.Duration)

	// MarkAuthError records a 403 / BANNED response. Disables the
	// credential until SetEnabled(id, true) is called.
	MarkAuthError(credID, reason string)

	// RefreshQuota updates the cached Kiro getUsageLimits snapshot and
	// clears LastQuotaError if non-empty. If the snapshot indicates a
	// ban, MarkAuthError is invoked as well.
	RefreshQuota(credID string, snap *KiroQuotaSnapshot)

	// RecordQuotaError stores a fetch failure for visibility in the admin
	// API without disabling the credential.
	RecordQuotaError(credID string, errMsg string)

	// SetEnabled toggles the operator-disabled flag. Setting enabled=true
	// clears DisabledReason / DisabledAt and resets the backoff exponent.
	SetEnabled(credID string, enabled bool) error

	// Add inserts a fresh credential into the pool. Returns ErrDuplicateID
	// if a credential with the same ID already exists.
	Add(cred *Credential) error

	// Remove deletes the credential with the given ID. Returns
	// ErrCredentialNotFound if no such credential is registered.
	Remove(credID string) error
}
