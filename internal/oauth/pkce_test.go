package oauth

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestNewVerifier_LengthAndCharset(t *testing.T) {
	v, err := NewVerifier()
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	// 32 bytes -> base64url no padding = 43 chars
	if len(v) != 43 {
		t.Errorf("expected 43-char verifier, got %d", len(v))
	}
	if strings.ContainsAny(v, "+/=") {
		t.Errorf("verifier contains non-url-safe characters: %q", v)
	}
}

func TestChallenge_S256Format(t *testing.T) {
	v := "test-verifier"
	c := Challenge(v)
	// S256 is 32 bytes -> base64url no padding = 43 chars
	if len(c) != 43 {
		t.Errorf("expected 43-char challenge, got %d", len(c))
	}
	// Must be decodable
	if _, err := base64.RawURLEncoding.DecodeString(c); err != nil {
		t.Errorf("challenge not valid base64url: %v", err)
	}
	// Deterministic
	if Challenge(v) != c {
		t.Errorf("challenge is non-deterministic")
	}
}

func TestNewState_LengthAndUnique(t *testing.T) {
	a, _ := NewState()
	b, _ := NewState()
	if a == b {
		t.Errorf("expected unique state nonces, got %q twice", a)
	}
	if len(a) < 30 {
		t.Errorf("state too short: %d chars", len(a))
	}
}

func TestStateCache_PutConsumeOnce(t *testing.T) {
	c := NewStateCache(time.Second)
	c.Put(StateEntry{State: "s1", Verifier: "v1", ProviderID: "kiro"})
	e, err := c.Consume("s1")
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if e.Verifier != "v1" {
		t.Errorf("verifier mismatch: %q", e.Verifier)
	}
	// Second consume must fail (used).
	if _, err := c.Consume("s1"); err != ErrStateNotFound {
		t.Errorf("expected ErrStateNotFound on second consume, got %v", err)
	}
}

func TestStateCache_Expiry(t *testing.T) {
	c := NewStateCache(20 * time.Millisecond)
	c.Put(StateEntry{State: "s2", Verifier: "v2", Created: time.Now().Add(-50 * time.Millisecond)})
	if _, err := c.Consume("s2"); err != ErrStateNotFound {
		t.Errorf("expected expired entry, got %v", err)
	}
}

func TestStateCache_Sweep(t *testing.T) {
	c := NewStateCache(20 * time.Millisecond)
	old := StateEntry{State: "old", Verifier: "v", Created: time.Now().Add(-1 * time.Hour)}
	fresh := StateEntry{State: "fresh", Verifier: "v"}
	c.Put(old)
	c.Put(fresh)
	if removed := c.Sweep(); removed != 1 {
		t.Errorf("expected 1 removed, got %d", removed)
	}
	if c.Size() != 1 {
		t.Errorf("expected 1 remaining, got %d", c.Size())
	}
}
