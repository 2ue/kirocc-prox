package pool

import (
	"context"
	"time"
)

// RefreshSkew is how far before expiry a credential is considered "stale"
// and triggers a preemptive refresh on Acquire.
const RefreshSkew = 5 * time.Minute

// Refresher knows how to refresh a single credential's OAuth tokens in
// place. Used by Conductor.Acquire to preempt token expiry.
type Refresher interface {
	// ShouldRefresh reports whether cred is within the refresh skew of
	// expiry (or already expired) and has the material needed to refresh.
	ShouldRefresh(cred *Credential) bool

	// Refresh updates cred.Credentials in place. Returns nil on success;
	// on error the in-memory credential is unchanged.
	Refresh(ctx context.Context, cred *Credential) error
}
