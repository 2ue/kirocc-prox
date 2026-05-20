package pool

import (
	"testing"
	"time"
)

func TestAffinity_GetReturnsFalseAfterTTL(t *testing.T) {
	a := NewAffinity(20 * time.Millisecond)
	a.Set("s1", "c1")
	if id, ok := a.Get("s1"); !ok || id != "c1" {
		t.Fatalf("immediate Get: id=%q ok=%v", id, ok)
	}
	time.Sleep(30 * time.Millisecond)
	if _, ok := a.Get("s1"); ok {
		t.Error("expected expired Get to return false")
	}
}

func TestAffinity_GetRefreshesTTL(t *testing.T) {
	a := NewAffinity(40 * time.Millisecond)
	a.Set("s1", "c1")
	time.Sleep(20 * time.Millisecond)
	// Refresh: should now be valid for another ~40ms.
	if _, ok := a.Get("s1"); !ok {
		t.Fatal("mid-life Get failed")
	}
	time.Sleep(25 * time.Millisecond)
	// Past original TTL but within refreshed TTL.
	if _, ok := a.Get("s1"); !ok {
		t.Error("Get should have refreshed the TTL")
	}
}

func TestAffinity_SweepRemovesExpired(t *testing.T) {
	a := NewAffinity(20 * time.Millisecond)
	a.Set("s1", "c1")
	a.Set("s2", "c2")
	if a.Size() != 2 {
		t.Fatalf("size = %d", a.Size())
	}
	time.Sleep(30 * time.Millisecond)
	removed := a.Sweep()
	if removed != 2 {
		t.Errorf("Sweep removed %d want 2", removed)
	}
	if a.Size() != 0 {
		t.Errorf("after Sweep size = %d want 0", a.Size())
	}
}

func TestAffinity_EmptySessionIgnored(t *testing.T) {
	a := NewAffinity(time.Minute)
	a.Set("", "c1")
	a.Set("s1", "")
	if a.Size() != 0 {
		t.Errorf("empty inputs must not populate: size = %d", a.Size())
	}
	if _, ok := a.Get(""); ok {
		t.Error("Get(\"\") must return false")
	}
}

func TestAffinity_ForgetRemoves(t *testing.T) {
	a := NewAffinity(time.Minute)
	a.Set("s1", "c1")
	a.Forget("s1")
	if _, ok := a.Get("s1"); ok {
		t.Error("Forget did not remove binding")
	}
}

func TestAffinity_DefaultTTL(t *testing.T) {
	a := NewAffinity(0)
	if a.ttl != 30*time.Minute {
		t.Errorf("default ttl = %s want 30m", a.ttl)
	}
}
