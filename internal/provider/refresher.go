package provider

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/niuma/kirocc-pro/internal/pool"
)

// MultiRefresher implements pool.Refresher by dispatching to the per-cred
// Provider.RefreshToken. After a successful refresh it atomically writes
// the full pool snapshot back to savePath (when non-empty) so the rotated
// tokens survive a restart.
type MultiRefresher struct {
	registry *Registry
	savePath string
	allCreds func() []*pool.Credential

	saveMu sync.Mutex // serializes disk writes
}

// NewMultiRefresher constructs a Refresher routing through registry. If
// savePath is empty, refreshes only update in-memory credentials.
func NewMultiRefresher(registry *Registry, savePath string, allCreds func() []*pool.Credential) *MultiRefresher {
	return &MultiRefresher{
		registry: registry,
		savePath: savePath,
		allCreds: allCreds,
	}
}

// ShouldRefresh reports whether cred is within pool.RefreshSkew of expiry
// and has the material to refresh.
func (r *MultiRefresher) ShouldRefresh(cred *pool.Credential) bool {
	cred.Mu.RLock()
	exp := cred.ExpiresAt
	hasRefresh := cred.RefreshToken != ""
	cred.Mu.RUnlock()
	if !hasRefresh || exp == 0 {
		return false
	}
	return time.Until(time.Unix(exp, 0)) < pool.RefreshSkew
}

// Refresh looks up the credential's provider and invokes its RefreshToken.
// On success the full pool is persisted to savePath. On provider lookup
// failure (unknown provider id), returns a wrapped error WITHOUT touching
// the credential.
func (r *MultiRefresher) Refresh(ctx context.Context, cred *pool.Credential) error {
	cred.Mu.RLock()
	provID := cred.Provider
	cred.Mu.RUnlock()
	if provID == "" {
		provID = "kiro"
	}
	p, err := r.registry.Get(provID)
	if err != nil {
		return fmt.Errorf("refresh %s: %w", provID, err)
	}
	if err := p.RefreshToken(ctx, cred); err != nil {
		return err
	}

	if r.savePath != "" && r.allCreds != nil {
		r.saveMu.Lock()
		defer r.saveMu.Unlock()
		if err := pool.SaveToJSON(r.savePath, r.allCreds()); err != nil {
			slog.WarnContext(ctx, "provider: persist refreshed creds failed",
				"path", r.savePath, "cred_id", cred.ID, "err", err)
		}
	}
	return nil
}
