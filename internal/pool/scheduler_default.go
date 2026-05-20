package pool

import (
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// DefaultScheduler is the standard Scheduler implementation. It guards the
// credential map with a sync.RWMutex; per-credential mutation is delegated
// to the credential's own Mu (see locking discipline on Credential).
type DefaultScheduler struct {
	mu    sync.RWMutex
	creds map[string]*Credential
	order []string // insertion order for deterministic iteration in All()
}

// NewDefaultScheduler creates an empty DefaultScheduler.
func NewDefaultScheduler() *DefaultScheduler {
	return &DefaultScheduler{
		creds: make(map[string]*Credential),
	}
}

// Register replaces the pool with the given credentials. Runtime state on
// previously-registered credentials with the same ID is copied into the new
// instance so cooldowns and counters survive a creds file reload.
func (s *DefaultScheduler) Register(creds []*Credential) {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := make(map[string]*Credential, len(creds))
	order := make([]string, 0, len(creds))
	for _, c := range creds {
		if c == nil || c.ID == "" {
			continue
		}
		if old, ok := s.creds[c.ID]; ok && old != c {
			copyRuntimeState(old, c)
		}
		next[c.ID] = c
		order = append(order, c.ID)
	}
	s.creds = next
	s.order = order
}

// copyRuntimeState transfers runtime fields (state below Mu) from src to dst.
// Both credentials are locked while the copy happens.
func copyRuntimeState(src, dst *Credential) {
	src.Mu.RLock()
	defer src.Mu.RUnlock()
	dst.Mu.Lock()
	defer dst.Mu.Unlock()

	dst.Quota = src.Quota
	dst.Success = src.Success
	dst.Failed = src.Failed
	dst.LastUsedAt = src.LastUsedAt
	dst.Disabled = src.Disabled
	dst.DisabledReason = src.DisabledReason
	dst.DisabledAt = src.DisabledAt
	dst.LastQuota = src.LastQuota
	dst.LastQuotaAt = src.LastQuotaAt
	dst.LastQuotaError = src.LastQuotaError
	dst.LastQuotaErrorAt = src.LastQuotaErrorAt

	if len(src.ModelStates) > 0 {
		dst.ModelStates = make(map[string]*ModelState, len(src.ModelStates))
		for k, ms := range src.ModelStates {
			cp := *ms
			dst.ModelStates[k] = &cp
		}
	}
}

// Ready returns credentials sorted by Priority descending (stable sort).
// Excludes Disabled and in-cooldown.
func (s *DefaultScheduler) Ready() []*Credential {
	s.mu.RLock()
	all := make([]*Credential, 0, len(s.creds))
	for _, id := range s.order {
		if c, ok := s.creds[id]; ok {
			all = append(all, c)
		}
	}
	s.mu.RUnlock()

	ready := make([]*Credential, 0, len(all))
	for _, c := range all {
		if c.IsReady() {
			ready = append(ready, c)
		}
	}
	sort.SliceStable(ready, func(i, j int) bool {
		return ready[i].Priority > ready[j].Priority
	})
	return ready
}

// Lookup returns the credential with the given ID, or nil if absent.
func (s *DefaultScheduler) Lookup(id string) *Credential {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.creds[id]
}

// All returns a snapshot of every registered credential.
func (s *DefaultScheduler) All() []*Credential {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Credential, 0, len(s.creds))
	for _, id := range s.order {
		if c, ok := s.creds[id]; ok {
			out = append(out, c)
		}
	}
	return out
}

// MarkSuccess resets backoff and bumps counters at both account and model level.
func (s *DefaultScheduler) MarkSuccess(credID, model string, u Usage) {
	c := s.Lookup(credID)
	if c == nil {
		return
	}
	c.Mu.Lock()
	defer c.Mu.Unlock()

	c.Quota.Exceeded = false
	c.Quota.BackoffLevel = 0
	c.Quota.NextRecoverAt = time.Time{}
	c.Success++
	c.LastUsedAt = time.Now()

	if model != "" {
		ms := ensureModelStateLocked(c, model)
		ms.Quota.Exceeded = false
		ms.Quota.BackoffLevel = 0
		ms.Quota.NextRecoverAt = time.Time{}
		ms.Success++
	}
}

// MarkRateLimit records a 429 / quota error. Schedules cooldown via NextBackoff
// on both account- and model-level state. If DisableCooling is set, only
// counters advance.
func (s *DefaultScheduler) MarkRateLimit(credID, model string, retryAfter time.Duration) {
	c := s.Lookup(credID)
	if c == nil {
		return
	}
	c.Mu.Lock()
	defer c.Mu.Unlock()

	c.Failed++

	if c.DisableCooling {
		slog.Debug("pool: rate limit ignored (cooling disabled)", "cred", credID, "model", model)
		if model != "" {
			ms := ensureModelStateLocked(c, model)
			ms.Failed++
		}
		return
	}

	d := NextBackoff(c.Quota.BackoffLevel, retryAfter)
	c.Quota.Exceeded = true
	c.Quota.NextRecoverAt = time.Now().Add(d)
	c.Quota.BackoffLevel++

	slog.Info("pool: credential entered cooldown",
		"cred", credID, "model", model,
		"duration", d, "level", c.Quota.BackoffLevel)

	if model != "" {
		ms := ensureModelStateLocked(c, model)
		md := NextBackoff(ms.Quota.BackoffLevel, retryAfter)
		ms.Quota.Exceeded = true
		ms.Quota.NextRecoverAt = time.Now().Add(md)
		ms.Quota.BackoffLevel++
		ms.Failed++
	}
}

// MarkAuthError disables the credential with a reason until SetEnabled(true).
func (s *DefaultScheduler) MarkAuthError(credID, reason string) {
	c := s.Lookup(credID)
	if c == nil {
		return
	}
	c.Mu.Lock()
	defer c.Mu.Unlock()
	c.Disabled = true
	c.DisabledReason = reason
	c.DisabledAt = time.Now()
	slog.Warn("pool: credential disabled by auth error", "cred", credID, "reason", reason)
}

// RefreshQuota updates the cached Kiro usage snapshot. If banned, also marks
// an auth error on the credential.
func (s *DefaultScheduler) RefreshQuota(credID string, snap *KiroQuotaSnapshot) {
	c := s.Lookup(credID)
	if c == nil {
		return
	}
	c.Mu.Lock()
	c.LastQuota = snap
	c.LastQuotaAt = time.Now()
	c.LastQuotaError = ""
	c.LastQuotaErrorAt = time.Time{}
	banned := snap != nil && snap.Banned
	reason := ""
	if banned {
		reason = snap.BanReason
		if reason == "" {
			reason = "banned"
		}
	}
	c.Mu.Unlock()

	if banned {
		s.MarkAuthError(credID, reason)
	}
}

// RecordQuotaError stores a fetch failure without disabling the credential.
func (s *DefaultScheduler) RecordQuotaError(credID string, errMsg string) {
	c := s.Lookup(credID)
	if c == nil {
		return
	}
	c.Mu.Lock()
	defer c.Mu.Unlock()
	c.LastQuotaError = errMsg
	c.LastQuotaErrorAt = time.Now()
}

// SetEnabled toggles the operator-disabled flag.
func (s *DefaultScheduler) SetEnabled(credID string, enabled bool) error {
	c := s.Lookup(credID)
	if c == nil {
		return ErrCredentialNotFound
	}
	c.Mu.Lock()
	defer c.Mu.Unlock()
	if enabled {
		c.Disabled = false
		c.DisabledReason = ""
		c.DisabledAt = time.Time{}
		c.Quota.BackoffLevel = 0
		c.Quota.Exceeded = false
		c.Quota.NextRecoverAt = time.Time{}
		return nil
	}
	c.Disabled = true
	if c.DisabledAt.IsZero() {
		c.DisabledAt = time.Now()
	}
	return nil
}

// Add inserts a fresh credential into the pool. Returns ErrDuplicateID
// if a credential with the same ID already exists.
func (s *DefaultScheduler) Add(cred *Credential) error {
	if cred == nil || cred.ID == "" {
		return errors.New("pool: nil credential or empty id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.creds[cred.ID]; ok {
		return ErrDuplicateID
	}
	s.creds[cred.ID] = cred
	s.order = append(s.order, cred.ID)
	return nil
}

// Remove deletes the credential with the given ID. Returns
// ErrCredentialNotFound if no such credential is registered.
func (s *DefaultScheduler) Remove(credID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.creds[credID]; !ok {
		return ErrCredentialNotFound
	}
	delete(s.creds, credID)
	for i, id := range s.order {
		if id == credID {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	return nil
}

// ensureModelStateLocked returns (creating if needed) the per-model state.
// Caller must hold c.Mu (write lock).
func ensureModelStateLocked(c *Credential, model string) *ModelState {
	if c.ModelStates == nil {
		c.ModelStates = make(map[string]*ModelState)
	}
	ms, ok := c.ModelStates[model]
	if !ok {
		ms = &ModelState{}
		c.ModelStates[model] = ms
	}
	return ms
}
