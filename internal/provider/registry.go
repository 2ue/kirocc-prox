package provider

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/niuma/kirocc-pro/internal/pool"
)

// ErrUnknownProvider is returned by Registry.Get / RouteFor when no
// matching provider is registered.
var ErrUnknownProvider = errors.New("provider: unknown")

// Registry holds the set of providers known to this proxy instance. It is
// safe for concurrent use; providers are typically registered once at
// startup and read frequently thereafter.
type Registry struct {
	mu       sync.RWMutex
	byID     map[string]Provider
	ordered  []Provider // registration order; routes are tried in this order
	fallback Provider   // used by RouteFor when no provider matches
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{byID: make(map[string]Provider)}
}

// Register adds p to the registry. The first-registered provider becomes
// the fallback for unmatched models. A provider with the same ID is
// silently replaced (caller is expected to register each provider once).
func (r *Registry) Register(p Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := p.ID()
	if _, ok := r.byID[id]; !ok {
		r.ordered = append(r.ordered, p)
	} else {
		// replace in ordered slice while keeping position
		for i, q := range r.ordered {
			if q.ID() == id {
				r.ordered[i] = p
				break
			}
		}
	}
	r.byID[id] = p
	if r.fallback == nil {
		r.fallback = p
	}
}

// SetFallback overrides the fallback provider used by RouteFor.
func (r *Registry) SetFallback(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.byID[id]
	if !ok {
		return ErrUnknownProvider
	}
	r.fallback = p
	return nil
}

// Get returns the provider with the given ID, or nil + ErrUnknownProvider.
func (r *Registry) Get(id string) (Provider, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if p, ok := r.byID[id]; ok {
		return p, nil
	}
	return nil, ErrUnknownProvider
}

// RouteFor returns the first registered provider whose HandlesModel
// matches the given model name. If none match, the fallback is returned.
// Returns nil only when no providers are registered at all.
func (r *Registry) RouteFor(model string) Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, p := range r.ordered {
		if p.HandlesModel(model) {
			return p
		}
	}
	return r.fallback
}

// All returns providers in registration order. The returned slice is a
// snapshot; mutating it does not affect the registry.
func (r *Registry) All() []Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Provider, len(r.ordered))
	copy(out, r.ordered)
	return out
}

// Len returns the count of registered providers.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.ordered)
}

// FetchQuota dispatches a quota query to the credential's provider. It lets
// callers such as the automatic quota poller use the same provider-aware path
// as the admin manual refresh flow, including per-account proxy handling.
func (r *Registry) FetchQuota(ctx context.Context, cred *pool.Credential) (*pool.KiroQuotaSnapshot, error) {
	if cred == nil {
		return nil, fmt.Errorf("provider: nil credential")
	}
	cred.Mu.RLock()
	provID := cred.Provider
	cred.Mu.RUnlock()
	if provID == "" {
		provID = "kiro"
	}
	p, err := r.Get(provID)
	if err != nil {
		return nil, fmt.Errorf("quota %s: %w", provID, err)
	}
	return p.FetchQuota(ctx, cred)
}
