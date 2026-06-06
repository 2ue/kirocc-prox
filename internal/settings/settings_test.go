package settings

import (
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/niuma/kirocc-pro/internal/promptcache"
)

func TestNew_CreatesDefaultsOnFirstRun(t *testing.T) {
	backend := newMemorySettingsBackend(nil)
	s, err := newWithBackend(backend)
	if err != nil {
		t.Fatalf("newWithBackend: %v", err)
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
	assertDefaultPromptCacheReports(t, cur.PromptCacheReports)
	if !strings.Contains(string(backend.data), `"prompt_cache_reports"`) {
		t.Fatalf("settings backend does not persist prompt_cache_reports: %s", backend.data)
	}
}

func TestStore_UpdatePersists(t *testing.T) {
	backend := newMemorySettingsBackend(nil)
	s, _ := newWithBackend(backend)

	if _, err := s.Update(func(c *Settings) error {
		c.System.Debug = true
		c.Network.RequestRetry = 7
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Re-open: backend should reflect changes.
	s2, _ := newWithBackend(backend)
	cur := s2.Get()
	if !cur.System.Debug {
		t.Error("debug not persisted")
	}
	if cur.Network.RequestRetry != 7 {
		t.Errorf("request_retry = %d", cur.Network.RequestRetry)
	}
}

func TestStore_UpdateRollsBackOnError(t *testing.T) {
	s, _ := newWithBackend(newMemorySettingsBackend(nil))
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

func TestNew_RejectsInvalidPromptCacheReports(t *testing.T) {
	_, err := newWithBackend(newMemorySettingsBackend([]byte(`{
		"schema_version": 1,
			"prompt_cache_reports": {
				"routes": {
					"custom-a": "profile-a",
					"/api/custom-a": "profile-b"
				},
				"profiles": {
					"profile-a": {"enabled": true},
					"profile-b": {"enabled": true}
				}
			}
		}`)))
	if err == nil {
		t.Fatal("expected invalid prompt_cache_reports error")
	}
	if !strings.Contains(err.Error(), "invalid prompt_cache_reports") {
		t.Fatalf("error = %v, want invalid prompt_cache_reports", err)
	}
}

func TestNew_MigratesMissingPromptCacheReportsToDefaults(t *testing.T) {
	backend := newMemorySettingsBackend([]byte(`{
		"schema_version": 1,
		"remote_management": {"allow_remote": false, "disable_panel": false},
		"api_keys": [],
		"system": {"debug": false, "commercial_mode": false, "logging_to_file": false, "usage_stats_enabled": true, "log_max_total_size_mb": 0},
		"network": {"request_retry": 3, "max_retry_creds": 0, "max_retry_interval": "30s", "routing_strategy": "round-robin", "session_affinity_ttl": "1h", "force_model_prefix": false, "session_sticky": true},
		"streaming": {"keepalive_seconds": 0, "bootstrap_retries": 1, "non_streaming_keepalive_seconds": 0},
		"optimizations": {}
	}`))

	s, err := newWithBackend(backend)
	if err != nil {
		t.Fatalf("newWithBackend: %v", err)
	}
	assertDefaultPromptCacheReports(t, s.Get().PromptCacheReports)
	if !strings.Contains(string(backend.data), `"prompt_cache_reports"`) {
		t.Fatalf("migrated settings did not persist prompt_cache_reports: %s", backend.data)
	}
}

func TestNew_ExplicitEmptyPromptCacheReportsDisablesDefaults(t *testing.T) {
	s, err := newWithBackend(newMemorySettingsBackend([]byte(`{
		"schema_version": 1,
		"prompt_cache_reports": {}
	}`)))
	if err != nil {
		t.Fatalf("newWithBackend: %v", err)
	}
	if !s.Get().PromptCacheReports.Empty() {
		t.Fatalf("prompt_cache_reports = %+v, want explicit empty config to stay disabled", s.Get().PromptCacheReports)
	}
	if _, ok := s.Get().PromptCacheReports.Match("/api/cc/v1/models"); ok {
		t.Fatal("explicit empty prompt_cache_reports should not match /api/cc/v1/models")
	}
}

func assertDefaultPromptCacheReports(t *testing.T, cfg promptcache.ReportConfig) {
	t.Helper()
	cfg = cfg.Normalized()
	if cfg.Empty() {
		t.Fatal("prompt_cache_reports is empty, want built-in defaults")
	}
	if got := cfg.Routes["/v1/messages"]; got != "default" {
		t.Fatalf("/v1/messages route = %q, want default", got)
	}
	for path, want := range map[string]string{
		"/api/cc/v1/models":   "cc",
		"/api/ha/v1/messages": "ha",
		"/api/na/v1/messages": "na",
	} {
		t.Run(path, func(t *testing.T) {
			got, ok := cfg.Match(path)
			if !ok {
				t.Fatalf("path %q did not match", path)
			}
			if got.Name != want {
				t.Fatalf("profile = %q, want %q", got.Name, want)
			}
		})
	}
}

type memorySettingsBackend struct {
	data []byte
}

func newMemorySettingsBackend(data []byte) *memorySettingsBackend {
	return &memorySettingsBackend{data: append([]byte(nil), data...)}
}

func (b *memorySettingsBackend) Load() ([]byte, error) {
	if len(b.data) == 0 {
		return nil, fs.ErrNotExist
	}
	return append([]byte(nil), b.data...), nil
}

func (b *memorySettingsBackend) Save(data []byte) error {
	b.data = append([]byte(nil), data...)
	return nil
}

func (b *memorySettingsBackend) Path() string { return "memory://settings" }

var errIntentional = errIntent{}

type errIntent struct{}

func (errIntent) Error() string { return "intentional" }

func TestAPIKey_AddListValidate(t *testing.T) {
	s, _ := newWithBackend(newMemorySettingsBackend(nil))

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
	s, _ := newWithBackend(newMemorySettingsBackend(nil))
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
	s, _ := newWithBackend(newMemorySettingsBackend(nil))
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
		"short":               "*****",
		"":                    "",
	}
	for in, want := range cases {
		if got := MaskAPIKey(in); got != want {
			t.Errorf("MaskAPIKey(%q) = %q, want %q", in, got, want)
		}
	}
}
