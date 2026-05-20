package pool

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrNoCredential is returned by Conductor.Acquire when neither affinity
// nor the selector can produce a credential.
var ErrNoCredential = errors.New("pool: no credential available")

// Conductor orchestrates credential selection with session affinity. Each
// proxy request:
//
//  1. Acquire(ctx, model, sessionID) returns a credential. If the session
//     is bound (affinity hit) and the bound credential is ready, that one
//     is reused (sticky semantics). If the bound credential is in cooldown,
//     the selector picks a temporary alternate WITHOUT rewriting affinity;
//     the next request after recovery returns to the bound credential.
//  2. Caller does the upstream work.
//  3. Caller reports the outcome via the Scheduler (MarkSuccess / etc.).
//  4. Release(cred) updates LastUsedAt and (in future) decrements an
//     in-flight counter.
type Conductor interface {
	Acquire(ctx context.Context, model, sessionID string) (*Credential, error)
	Release(cred *Credential)
}

// DefaultConductor implementation is provided in conductor_default.go.

// Affinity is an in-memory sticky session map (sessionID → credentialID).
// Entries expire after TTL of inactivity. Safe for concurrent use.
type Affinity struct {
	ttl     time.Duration
	mu      sync.Mutex
	entries map[string]affinityEntry
}

type affinityEntry struct {
	CredID   string
	ExpireAt time.Time
}

// NewAffinity constructs an Affinity with the given TTL. Recommended: 30 min.
func NewAffinity(ttl time.Duration) *Affinity {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	return &Affinity{
		ttl:     ttl,
		entries: make(map[string]affinityEntry),
	}
}

// Get returns the credential ID bound to sessionID (and resets its expiry),
// or "", false if no binding exists or the binding has expired.
func (a *Affinity) Get(sessionID string) (string, bool) {
	if sessionID == "" {
		return "", false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.entries[sessionID]
	if !ok {
		return "", false
	}
	if time.Now().After(e.ExpireAt) {
		delete(a.entries, sessionID)
		return "", false
	}
	e.ExpireAt = time.Now().Add(a.ttl)
	a.entries[sessionID] = e
	return e.CredID, true
}

// Set binds sessionID to credID, replacing any previous binding.
func (a *Affinity) Set(sessionID, credID string) {
	if sessionID == "" || credID == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries[sessionID] = affinityEntry{
		CredID:   credID,
		ExpireAt: time.Now().Add(a.ttl),
	}
}

// Forget removes the binding for sessionID, if any.
func (a *Affinity) Forget(sessionID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.entries, sessionID)
}

// Sweep removes expired entries. Callers should invoke this periodically
// (a background goroutine in production; in tests, manually).
func (a *Affinity) Sweep() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	removed := 0
	for k, e := range a.entries {
		if now.After(e.ExpireAt) {
			delete(a.entries, k)
			removed++
		}
	}
	return removed
}

// Size returns the current number of live bindings. For tests / metrics.
func (a *Affinity) Size() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.entries)
}
