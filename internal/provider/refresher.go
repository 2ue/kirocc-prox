package provider

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/niuma/kirocc-pro/internal/pool"
)

// MultiRefresher implements pool.Refresher by dispatching to the per-cred
// Provider.RefreshToken. After a successful refresh it persists the rotated
// credential through the configured durable account store.
type MultiRefresher struct {
	registry *Registry
	saveOne  func(context.Context, *pool.Credential) error
}

func NewMultiRefresherWithPersister(registry *Registry, saveOne func(context.Context, *pool.Credential) error) *MultiRefresher {
	return &MultiRefresher{
		registry: registry,
		saveOne:  saveOne,
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
// On success the rotated credential is persisted to PostgreSQL. On provider
// lookup failure (unknown provider id), returns a wrapped error WITHOUT
// touching the credential.
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

	if r.saveOne != nil {
		if err := r.saveOne(ctx, cred); err != nil {
			slog.WarnContext(ctx, "provider: persist refreshed credential failed",
				"cred_id", cred.ID, "err", err)
		}
	}
	return nil
}
