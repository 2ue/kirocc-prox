package pool

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func newCred(id string, priority int) *Credential {
	return &Credential{ID: id, Priority: priority}
}

func TestScheduler_RegisterPreservesState(t *testing.T) {
	s := NewDefaultScheduler()
	c1 := newCred("a", 100)
	s.Register([]*Credential{c1})

	s.MarkSuccess("a", "claude", Usage{InputTokens: 10})
	s.MarkRateLimit("a", "claude", 0)

	c1.Mu.RLock()
	wantSuccess := c1.Success
	wantFailed := c1.Failed
	wantLevel := c1.Quota.BackoffLevel
	wantNext := c1.Quota.NextRecoverAt
	c1.Mu.RUnlock()

	// Re-register with a fresh instance with the same ID.
	c2 := newCred("a", 100)
	s.Register([]*Credential{c2})

	c2.Mu.RLock()
	defer c2.Mu.RUnlock()
	if c2.Success != wantSuccess {
		t.Errorf("Success: got %d want %d", c2.Success, wantSuccess)
	}
	if c2.Failed != wantFailed {
		t.Errorf("Failed: got %d want %d", c2.Failed, wantFailed)
	}
	if c2.Quota.BackoffLevel != wantLevel {
		t.Errorf("BackoffLevel: got %d want %d", c2.Quota.BackoffLevel, wantLevel)
	}
	if !c2.Quota.NextRecoverAt.Equal(wantNext) {
		t.Errorf("NextRecoverAt mismatch: got %v want %v", c2.Quota.NextRecoverAt, wantNext)
	}
	if c2.ModelStates["claude"] == nil {
		t.Fatal("ModelStates[claude] not preserved")
	}
}

func TestScheduler_MarkSuccessResetsBackoff(t *testing.T) {
	s := NewDefaultScheduler()
	c := newCred("a", 100)
	s.Register([]*Credential{c})

	s.MarkRateLimit("a", "m", 0)
	s.MarkRateLimit("a", "m", 0)

	c.Mu.RLock()
	if c.Quota.BackoffLevel == 0 {
		t.Fatal("expected non-zero backoff level after rate limits")
	}
	c.Mu.RUnlock()

	s.MarkSuccess("a", "m", Usage{})
	c.Mu.RLock()
	defer c.Mu.RUnlock()
	if c.Quota.BackoffLevel != 0 {
		t.Errorf("backoff level should reset to 0, got %d", c.Quota.BackoffLevel)
	}
	if c.Quota.Exceeded {
		t.Error("Exceeded should be false after success")
	}
	if c.ModelStates["m"].Quota.BackoffLevel != 0 {
		t.Errorf("model backoff level should reset to 0, got %d", c.ModelStates["m"].Quota.BackoffLevel)
	}
}

func TestScheduler_MarkRateLimitIncrements(t *testing.T) {
	s := NewDefaultScheduler()
	c := newCred("a", 100)
	s.Register([]*Credential{c})

	s.MarkRateLimit("a", "m", 0)
	c.Mu.RLock()
	level1 := c.Quota.BackoffLevel
	c.Mu.RUnlock()

	s.MarkRateLimit("a", "m", 0)
	c.Mu.RLock()
	defer c.Mu.RUnlock()
	if c.Quota.BackoffLevel != level1+1 {
		t.Errorf("level should increment: got %d want %d", c.Quota.BackoffLevel, level1+1)
	}
	if !c.Quota.Exceeded {
		t.Error("Exceeded should be true")
	}
	if c.Quota.NextRecoverAt.Before(time.Now()) {
		t.Error("NextRecoverAt should be in the future")
	}
}

func TestScheduler_MarkRateLimit_DisableCooling(t *testing.T) {
	s := NewDefaultScheduler()
	c := newCred("a", 100)
	c.DisableCooling = true
	s.Register([]*Credential{c})

	s.MarkRateLimit("a", "m", 0)

	c.Mu.RLock()
	defer c.Mu.RUnlock()
	if c.Quota.Exceeded {
		t.Error("DisableCooling: Exceeded should remain false")
	}
	if c.Quota.BackoffLevel != 0 {
		t.Errorf("DisableCooling: BackoffLevel should remain 0, got %d", c.Quota.BackoffLevel)
	}
	if c.Failed != 1 {
		t.Errorf("Failed counter should still increment, got %d", c.Failed)
	}
}

func TestScheduler_MarkAuthErrorDisables(t *testing.T) {
	s := NewDefaultScheduler()
	c := newCred("a", 100)
	s.Register([]*Credential{c})

	s.MarkAuthError("a", "BANNED: abuse")

	c.Mu.RLock()
	defer c.Mu.RUnlock()
	if !c.Disabled {
		t.Error("expected Disabled=true")
	}
	if c.DisabledReason != "BANNED: abuse" {
		t.Errorf("DisabledReason = %q", c.DisabledReason)
	}
	if c.DisabledAt.IsZero() {
		t.Error("DisabledAt should be set")
	}
}

func TestScheduler_SetEnabledReEnables(t *testing.T) {
	s := NewDefaultScheduler()
	c := newCred("a", 100)
	s.Register([]*Credential{c})

	s.MarkAuthError("a", "test")
	s.MarkRateLimit("a", "m", 0)
	s.MarkRateLimit("a", "m", 0)

	if err := s.SetEnabled("a", true); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}

	c.Mu.RLock()
	defer c.Mu.RUnlock()
	if c.Disabled {
		t.Error("Disabled should be cleared")
	}
	if c.DisabledReason != "" {
		t.Errorf("DisabledReason should be cleared, got %q", c.DisabledReason)
	}
	if !c.DisabledAt.IsZero() {
		t.Error("DisabledAt should be cleared")
	}
	if c.Quota.BackoffLevel != 0 {
		t.Errorf("BackoffLevel should be reset, got %d", c.Quota.BackoffLevel)
	}
	if c.Quota.Exceeded {
		t.Error("Exceeded should be cleared")
	}
}

func TestScheduler_SetEnabled_NotFound(t *testing.T) {
	s := NewDefaultScheduler()
	if err := s.SetEnabled("missing", true); !errors.Is(err, ErrCredentialNotFound) {
		t.Errorf("got %v want ErrCredentialNotFound", err)
	}
}

func TestScheduler_ReadyExcludesDisabledAndCooldown(t *testing.T) {
	s := NewDefaultScheduler()
	a := newCred("a", 100)
	b := newCred("b", 90)
	c := newCred("c", 80)
	s.Register([]*Credential{a, b, c})

	// disable a; cooldown b; c stays ready.
	s.MarkAuthError("a", "test")
	s.MarkRateLimit("b", "", 0)

	ready := s.Ready()
	if len(ready) != 1 {
		t.Fatalf("Ready len = %d want 1: %+v", len(ready), credIDs(ready))
	}
	if ready[0].ID != "c" {
		t.Errorf("expected c, got %s", ready[0].ID)
	}
}

func TestScheduler_ReadySortedByPriorityDesc(t *testing.T) {
	s := NewDefaultScheduler()
	a := newCred("a", 50)
	b := newCred("b", 100)
	c := newCred("c", 75)
	s.Register([]*Credential{a, b, c})

	got := s.Ready()
	if len(got) != 3 {
		t.Fatalf("Ready len = %d", len(got))
	}
	want := []string{"b", "c", "a"}
	for i, id := range want {
		if got[i].ID != id {
			t.Errorf("pos %d: got %s want %s", i, got[i].ID, id)
		}
	}
}

func TestScheduler_RefreshQuota_BannedDisables(t *testing.T) {
	s := NewDefaultScheduler()
	c := newCred("a", 100)
	s.Register([]*Credential{c})

	snap := &KiroQuotaSnapshot{Banned: true, BanReason: "BANNED: abuse"}
	s.RefreshQuota("a", snap)

	c.Mu.RLock()
	defer c.Mu.RUnlock()
	if !c.Disabled {
		t.Error("expected Disabled=true after banned snapshot")
	}
	if c.LastQuota != snap {
		t.Error("LastQuota not stored")
	}
	if c.DisabledReason != "BANNED: abuse" {
		t.Errorf("reason = %q", c.DisabledReason)
	}
}

func TestScheduler_RecordQuotaError(t *testing.T) {
	s := NewDefaultScheduler()
	c := newCred("a", 100)
	s.Register([]*Credential{c})

	s.RecordQuotaError("a", "boom")
	c.Mu.RLock()
	defer c.Mu.RUnlock()
	if c.LastQuotaError != "boom" {
		t.Errorf("got %q want boom", c.LastQuotaError)
	}
	if c.Disabled {
		t.Error("RecordQuotaError should NOT disable")
	}
}

func TestScheduler_ConcurrentMarks(t *testing.T) {
	s := NewDefaultScheduler()
	c := newCred("a", 100)
	s.Register([]*Credential{c})

	const N = 200
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < N; i++ {
			s.MarkSuccess("a", "m", Usage{})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < N; i++ {
			_ = s.Ready()
		}
	}()
	wg.Wait()

	c.Mu.RLock()
	defer c.Mu.RUnlock()
	if c.Success != int64(N) {
		t.Errorf("Success = %d want %d", c.Success, N)
	}
}

func credIDs(creds []*Credential) []string {
	out := make([]string, len(creds))
	for i, c := range creds {
		out[i] = c.ID
	}
	return out
}
