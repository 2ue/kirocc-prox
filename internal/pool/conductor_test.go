package pool

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestConductor_AffinityHitReuses(t *testing.T) {
	s := NewDefaultScheduler()
	a := newCred("a", 100)
	b := newCred("b", 100)
	s.Register([]*Credential{a, b})

	aff := NewAffinity(5 * time.Minute)
	c := NewConductor(s, &RoundRobinSelector{}, aff)

	// First Acquire binds session -> some cred.
	first, err := c.Acquire(context.Background(), "m", "sess-1")
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	// Second Acquire with same session must return the same cred,
	// regardless of round-robin advance.
	for i := 0; i < 4; i++ {
		got, err := c.Acquire(context.Background(), "m", "sess-1")
		if err != nil {
			t.Fatalf("Acquire %d: %v", i, err)
		}
		if got != first {
			t.Errorf("iter %d: affinity broken (got %s want %s)", i, got.ID, first.ID)
		}
	}
}

func TestConductor_AffinityMissPicksFreshAndBinds(t *testing.T) {
	s := NewDefaultScheduler()
	a := newCred("a", 100)
	s.Register([]*Credential{a})

	aff := NewAffinity(5 * time.Minute)
	c := NewConductor(s, &FillFirstSelector{}, aff)

	if aff.Size() != 0 {
		t.Fatalf("pre: affinity size = %d", aff.Size())
	}
	got, err := c.Acquire(context.Background(), "m", "sess-1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if got != a {
		t.Errorf("expected a, got %s", got.ID)
	}
	if aff.Size() != 1 {
		t.Errorf("post: affinity size = %d want 1", aff.Size())
	}
	id, ok := aff.Get("sess-1")
	if !ok || id != "a" {
		t.Errorf("affinity binding wrong: id=%q ok=%v", id, ok)
	}
}

func TestConductor_AffinityHitOnCooldownFallsThroughNoUpdate(t *testing.T) {
	s := NewDefaultScheduler()
	a := newCred("a", 100)
	b := newCred("b", 90)
	s.Register([]*Credential{a, b})

	aff := NewAffinity(5 * time.Minute)
	c := NewConductor(s, &FillFirstSelector{}, aff)

	// Bind sess-1 -> a (highest priority).
	first, err := c.Acquire(context.Background(), "m", "sess-1")
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	if first != a {
		t.Fatalf("expected first pick to be a, got %s", first.ID)
	}

	// Put a into cooldown.
	s.MarkRateLimit("a", "", 0)

	// Next Acquire with same session: a is bound but in cooldown.
	// Selector should pick b. Affinity must NOT be rewritten.
	got, err := c.Acquire(context.Background(), "m", "sess-1")
	if err != nil {
		t.Fatalf("second Acquire: %v", err)
	}
	if got != b {
		t.Errorf("expected fallback to b, got %s", got.ID)
	}
	id, ok := aff.Get("sess-1")
	if !ok || id != "a" {
		t.Errorf("affinity must still point to a: id=%q ok=%v", id, ok)
	}
}

func TestConductor_NoSession_NoAffinityWrite(t *testing.T) {
	s := NewDefaultScheduler()
	s.Register([]*Credential{newCred("a", 100)})
	aff := NewAffinity(5 * time.Minute)
	c := NewConductor(s, &FillFirstSelector{}, aff)

	if _, err := c.Acquire(context.Background(), "m", ""); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if aff.Size() != 0 {
		t.Errorf("empty session must not write affinity (size=%d)", aff.Size())
	}
}

func TestConductor_NilAffinity(t *testing.T) {
	s := NewDefaultScheduler()
	s.Register([]*Credential{newCred("a", 100)})
	c := NewConductor(s, &FillFirstSelector{}, nil)

	got, err := c.Acquire(context.Background(), "m", "sess-1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if got.ID != "a" {
		t.Errorf("got %s", got.ID)
	}
}

func TestConductor_NoneReady(t *testing.T) {
	s := NewDefaultScheduler()
	a := newCred("a", 100)
	s.Register([]*Credential{a})
	s.MarkAuthError("a", "test")

	c := NewConductor(s, &FillFirstSelector{}, NewAffinity(time.Minute))
	if _, err := c.Acquire(context.Background(), "m", "sess-1"); !errors.Is(err, ErrNoReady) {
		t.Errorf("got %v want ErrNoReady", err)
	}
}

func TestConductor_RegionHintFallsBackAfterModelCooldown(t *testing.T) {
	s := NewDefaultScheduler()
	local := newCred("local", 100)
	local.Region = "us-east-1"
	local.ModelStates = map[string]*ModelState{
		"claude-sonnet": {
			Quota: QuotaState{
				Exceeded:      true,
				NextRecoverAt: time.Now().Add(time.Minute),
			},
		},
	}
	remote := newCred("remote", 90)
	remote.Region = "eu-west-1"
	s.Register([]*Credential{local, remote})

	c := NewConductor(s, &FillFirstSelector{}, nil)
	got, err := c.Acquire(WithRegionHint(context.Background(), "us-east-1"), "claude-sonnet", "")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if got != remote {
		t.Fatalf("got %s want remote fallback", got.ID)
	}
}

func TestConductor_Release(t *testing.T) {
	s := NewDefaultScheduler()
	a := newCred("a", 100)
	s.Register([]*Credential{a})

	c := NewConductor(s, &FillFirstSelector{}, nil)
	got, _ := c.Acquire(context.Background(), "m", "")
	before := time.Now().Add(-time.Second)
	c.Release(got)

	a.Mu.RLock()
	defer a.Mu.RUnlock()
	if !a.LastUsedAt.After(before) {
		t.Errorf("LastUsedAt not updated: %v", a.LastUsedAt)
	}

	// Nil release is a no-op.
	c.Release(nil)
}

func TestConductor_AcquireReleaseTracksInFlight(t *testing.T) {
	s := NewDefaultScheduler()
	a := newCred("a", 100)
	s.Register([]*Credential{a})

	c := NewConductor(s, &FillFirstSelector{}, nil)
	got, err := c.Acquire(context.Background(), "m", "")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if got != a {
		t.Fatalf("got %s want a", got.ID)
	}

	a.Mu.RLock()
	inFlight := a.InFlight
	modelInFlight := a.InFlightByModel["m"]
	a.Mu.RUnlock()
	if inFlight != 1 {
		t.Fatalf("InFlight after acquire = %d want 1", inFlight)
	}
	if modelInFlight != 1 {
		t.Fatalf("InFlightByModel[m] after acquire = %d want 1", modelInFlight)
	}

	c.Release(got, "m")
	a.Mu.RLock()
	defer a.Mu.RUnlock()
	if a.InFlight != 0 {
		t.Errorf("InFlight after release = %d want 0", a.InFlight)
	}
	if _, ok := a.InFlightByModel["m"]; ok {
		t.Errorf("InFlightByModel[m] should be removed after release")
	}
}

func TestConductor_MaxInFlightGatesAcquire(t *testing.T) {
	s := NewDefaultScheduler()
	a := newCred("a", 100)
	a.MaxInFlight = 1
	s.Register([]*Credential{a})

	c := NewConductor(s, &FillFirstSelector{}, nil)
	first, err := c.Acquire(context.Background(), "m", "")
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	if _, err := c.Acquire(context.Background(), "m", ""); !errors.Is(err, ErrNoReady) {
		t.Fatalf("second Acquire err = %v want ErrNoReady", err)
	}

	c.Release(first, "m")
	if _, err := c.Acquire(context.Background(), "m", ""); err != nil {
		t.Fatalf("Acquire after release: %v", err)
	}
}

func TestConductor_ReleaseAfterRegisterDecrementsCurrentCredential(t *testing.T) {
	s := NewDefaultScheduler()
	old := newCred("a", 100)
	s.Register([]*Credential{old})

	c := NewConductor(s, &FillFirstSelector{}, nil)
	got, err := c.Acquire(context.Background(), "m", "")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if got != old {
		t.Fatalf("got %s want old", got.ID)
	}

	current := newCred("a", 100)
	s.Register([]*Credential{current})

	current.Mu.RLock()
	inFlight := current.InFlight
	modelInFlight := current.InFlightByModel["m"]
	current.Mu.RUnlock()
	if inFlight != 1 || modelInFlight != 1 {
		t.Fatalf("copied in-flight = %d/%d want 1/1", inFlight, modelInFlight)
	}

	c.Release(got, "m")

	current.Mu.RLock()
	defer current.Mu.RUnlock()
	if current.InFlight != 0 {
		t.Errorf("current InFlight after stale release = %d want 0", current.InFlight)
	}
	if _, ok := current.InFlightByModel["m"]; ok {
		t.Errorf("current InFlightByModel[m] should be removed after stale release")
	}
}

func TestConductor_ReleaseAfterRemoveAndReaddDoesNotDecrementNewCredential(t *testing.T) {
	s := NewDefaultScheduler()
	old := newCred("a", 100)
	s.Register([]*Credential{old})

	c := NewConductor(s, &FillFirstSelector{}, nil)
	got, err := c.Acquire(context.Background(), "m", "")
	if err != nil {
		t.Fatalf("Acquire old: %v", err)
	}

	if err := s.Remove("a"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	fresh := newCred("a", 100)
	if err := s.Add(fresh); err != nil {
		t.Fatalf("Add fresh: %v", err)
	}
	if !fresh.Reserve("m") {
		t.Fatal("Reserve fresh returned false")
	}

	c.Release(got, "m")

	fresh.Mu.RLock()
	defer fresh.Mu.RUnlock()
	if fresh.InFlight != 1 {
		t.Errorf("fresh InFlight = %d want 1", fresh.InFlight)
	}
	if got := fresh.InFlightByModel["m"]; got != 1 {
		t.Errorf("fresh InFlightByModel[m] = %d want 1", got)
	}
}
