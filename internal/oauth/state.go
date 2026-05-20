package oauth

import (
	"errors"
	"sync"
	"time"
)

// DefaultStateTTL is the lifetime of an authorization-request state entry.
// 10 minutes is enough for a slow human login (including 2FA) but short
// enough that an abandoned flow is cleaned up.
const DefaultStateTTL = 10 * time.Minute

// ErrStateNotFound is returned by StateCache.Consume when the state is
// unknown or has expired.
var ErrStateNotFound = errors.New("oauth: state not found or expired")

// StateEntry is the per-flow data persisted from /oauth/start through to
// /oauth/callback. Implementations of provider-specific OAuth callbacks
// can stash extra fields by embedding StateEntry in their own type.
type StateEntry struct {
	State       string
	Verifier    string
	ProviderID  string // which Provider initiated this flow
	RedirectURI string // exact callback URL passed to authorization server
	Created     time.Time
	Extra       map[string]string // provider-specific bag (region, label hint, etc.)
}

// StateCache is an in-memory TTL'd store keyed by the OAuth state nonce.
// Safe for concurrent use. Entries auto-expire on read.
type StateCache struct {
	ttl     time.Duration
	mu      sync.Mutex
	entries map[string]StateEntry
}

// NewStateCache returns a StateCache with the given TTL. ttl <= 0 falls
// back to DefaultStateTTL.
func NewStateCache(ttl time.Duration) *StateCache {
	if ttl <= 0 {
		ttl = DefaultStateTTL
	}
	return &StateCache{ttl: ttl, entries: make(map[string]StateEntry)}
}

// Put stores entry under entry.State. Replaces any prior entry with the
// same state.
func (c *StateCache) Put(entry StateEntry) {
	if entry.Created.IsZero() {
		entry.Created = time.Now()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[entry.State] = entry
}

// Consume returns the entry for state and removes it from the cache.
// Returns ErrStateNotFound if the entry is absent or expired.
func (c *StateCache) Consume(state string) (StateEntry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[state]
	if !ok {
		return StateEntry{}, ErrStateNotFound
	}
	delete(c.entries, state)
	if time.Since(e.Created) > c.ttl {
		return StateEntry{}, ErrStateNotFound
	}
	return e, nil
}

// Sweep removes expired entries; intended to be invoked periodically.
// Returns the number of entries removed.
func (c *StateCache) Sweep() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	removed := 0
	for k, e := range c.entries {
		if now.Sub(e.Created) > c.ttl {
			delete(c.entries, k)
			removed++
		}
	}
	return removed
}

// Size returns the current count (post-sweep is not implied; callers may
// want to call Sweep first).
func (c *StateCache) Size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
