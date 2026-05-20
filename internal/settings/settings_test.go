package settings

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNew_CreatesDefaultsOnFirstRun(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	s, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected file written: %v", err)
	}
	cur := s.Get()
	if cur.SchemaVersion != CurrentSchemaVersion {
		t.Errorf("schema version = %d", cur.SchemaVersion)
	}
	if cur.Network.RequestRetry != 3 {
		t.Errorf("default request_retry = %d, want 3", cur.Network.RequestRetry)
	}
	if cur.Network.MaxRetryInterval() != 30*time.Second {
		t.Errorf("default max_retry_interval = %s", cur.Network.MaxRetryInterval())
	}
	if cur.Network.RoutingStrategy != "round-robin" {
		t.Errorf("default routing = %q", cur.Network.RoutingStrategy)
	}
}

func TestStore_UpdatePersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	s, _ := New(path)

	if _, err := s.Update(func(c *Settings) error {
		c.System.Debug = true
		c.Network.RequestRetry = 7
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Re-open: file should reflect changes.
	s2, _ := New(path)
	cur := s2.Get()
	if !cur.System.Debug {
		t.Error("debug not persisted")
	}
	if cur.Network.RequestRetry != 7 {
		t.Errorf("request_retry = %d", cur.Network.RequestRetry)
	}
}

func TestStore_UpdateRollsBackOnError(t *testing.T) {
	s, _ := New(filepath.Join(t.TempDir(), "s.json"))
	prevRetry := s.Get().Network.RequestRetry

	_, err := s.Update(func(c *Settings) error {
		c.Network.RequestRetry = 99
		return errIntentional
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if got := s.Get().Network.RequestRetry; got != prevRetry {
		t.Errorf("rolled-back retry = %d, want %d", got, prevRetry)
	}
}

var errIntentional = errIntent{}

type errIntent struct{}

func (errIntent) Error() string { return "intentional" }

func TestAPIKey_AddListValidate(t *testing.T) {
	s, _ := New(filepath.Join(t.TempDir(), "s.json"))

	k1, err := s.AddAPIKey(APIKeyOptions{Label: "alice"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if k1.Key == "" || k1.ID == "" {
		t.Fatal("empty key or id")
	}

	if got, err := s.ValidateAPIKey(k1.Key); err != nil || got.ID != k1.ID {
		t.Errorf("Validate first key: id=%q err=%v", got.ID, err)
	}
	if _, err := s.ValidateAPIKey("sk-bogus"); err == nil {
		t.Error("expected miss for bogus key")
	}
}

func TestAPIKey_RotateAndDelete(t *testing.T) {
	s, _ := New(filepath.Join(t.TempDir(), "s.json"))
	k, _ := s.AddAPIKey(APIKeyOptions{Label: "bob"})

	newVal, err := s.RotateAPIKey(k.ID)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if newVal == k.Key {
		t.Error("rotated key value did not change")
	}
	if _, err := s.ValidateAPIKey(k.Key); err == nil {
		t.Error("old value still valid after rotate")
	}
	if _, err := s.ValidateAPIKey(newVal); err != nil {
		t.Errorf("new value invalid: %v", err)
	}

	if err := s.DeleteAPIKey(k.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.ValidateAPIKey(newVal); err == nil {
		t.Error("deleted key still valid")
	}
}

func TestAPIKey_DisabledRejected(t *testing.T) {
	s, _ := New(filepath.Join(t.TempDir(), "s.json"))
	k, _ := s.AddAPIKey(APIKeyOptions{Label: "c"})
	disabled := false
	if err := s.UpdateAPIKey(k.ID, APIKeyPatch{Enabled: &disabled}); err != nil {
		t.Fatalf("UpdateAPIKey: %v", err)
	}
	if _, err := s.ValidateAPIKey(k.Key); err == nil {
		t.Error("disabled key should not validate")
	}
}

func TestMaskAPIKey(t *testing.T) {
	cases := map[string]string{
		"sk-abcdef1234567890": "sk******90",
		"short":                "*****",
		"":                     "",
	}
	for in, want := range cases {
		if got := MaskAPIKey(in); got != want {
			t.Errorf("MaskAPIKey(%q) = %q, want %q", in, got, want)
		}
	}
}
