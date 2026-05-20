// Package settings is the runtime-mutable configuration store for
// kirocc-pro. Unlike `internal/config` which captures startup flags +
// env vars (immutable for the process lifetime), this package owns
// values that can change while the server is running — driven by the
// admin UI and persisted to disk so they survive restarts.
//
// Schema is JSON on disk; in-memory we keep a *Settings behind a
// RWMutex. Access is goroutine-safe via Get/Update.
//
// Defaults match the project's flag defaults so a fresh user gets the
// same behavior whether or not settings.json exists yet.
package settings

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Settings is the persisted, runtime-mutable configuration.
type Settings struct {
	// §3 Remote management. AllowRemote=true allows binding to a
	// non-loopback host AND requires MgmtKey to be set. DisablePanel
	// hides the embedded UI (only the JSON API responds).
	RemoteManagement RemoteManagement `json:"remote_management"`

	// §4 Auth. Multiple API keys may be active at once; the proxy
	// accepts any one of them on /v1/messages.
	APIKeys []APIKey `json:"api_keys"`

	// §5 System.
	System System `json:"system"`

	// §6 Network. Proxy & retry knobs that previously lived as flags
	// or as hard-coded constants.
	Network Network `json:"network"`

	// §8 Streaming. Keepalive & bootstrap-retry tuning.
	Streaming Streaming `json:"streaming"`

	// §9 Optimizations. Runtime-tweakable values for kirocc's fork
	// fixes — currently the thinking-budget floor + experimental
	// thinking/system knobs.
	Optimizations Optimizations `json:"optimizations"`

	// schemaVersion guards against breaking format changes; bump when
	// removing or repurposing fields.
	SchemaVersion int `json:"schema_version"`
}

// Optimizations carries values for kirocc's runtime-tweakable proxy
// knobs (fix #8 + thinking / experimental switches). On startup, values
// here populate the corresponding KIROCC_* environment variables IFF
// that env var is unset, so explicit env overrides still win.
type Optimizations struct {
	// ForceThinkingBudget mirrors KIROCC_FORCE_THINKING_BUDGET (int).
	// 0 = unset (no floor injected).
	ForceThinkingBudget int `json:"force_thinking_budget,omitempty"`

	// ThinkingPromptMode mirrors KIROCC_EXPERIMENT_THINKING_PROMPT.
	// "" | "tool" | "minimal"
	ThinkingPromptMode string `json:"thinking_prompt_mode,omitempty"`

	// ThinkingToolContinueMode mirrors KIROCC_EXPERIMENT_THINKING_TOOL_CONTINUE.
	// "" | "assistant_only"
	ThinkingToolContinueMode string `json:"thinking_tool_continue_mode,omitempty"`

	// SystemMode mirrors KIROCC_EXPERIMENT_SYSTEM_MODE.
	SystemMode string `json:"system_mode,omitempty"`

	// UpstreamOrigin mirrors KIROCC_UPSTREAM_ORIGIN.
	UpstreamOrigin string `json:"upstream_origin,omitempty"`

	// ModelMappings mirrors KIROCC_MODEL_MAPPINGS (JSON object).
	ModelMappings string `json:"model_mappings,omitempty"`

	// AuditLog mirrors KIROCC_AUDIT_LOG (file path).
	AuditLog string `json:"audit_log,omitempty"`
}

// RemoteManagement covers section 3.
type RemoteManagement struct {
	AllowRemote   bool   `json:"allow_remote"`
	DisablePanel  bool   `json:"disable_panel"`
	MgmtKey       string `json:"mgmt_key,omitempty"`
	PanelRepoURL  string `json:"panel_repo_url,omitempty"`
}

// APIKey is one entry in §4.
type APIKey struct {
	ID         string `json:"id"`                      // stable opaque id
	Label      string `json:"label"`                   // human label "alice's laptop"
	Key        string `json:"key"`                     // the actual secret
	Enabled    bool   `json:"enabled"`
	CreatedAt  int64  `json:"created_at"`              // unix seconds
	ExpiresAt  int64  `json:"expires_at,omitempty"`    // unix seconds; 0 = never expires
	QuotaLimit int64  `json:"quota_limit,omitempty"`   // total input+output tokens allowed; 0 = unlimited
	UsedTokens int64  `json:"used_tokens,omitempty"`   // input+output tokens consumed so far
}

// System covers §5.
type System struct {
	Debug              bool `json:"debug"`
	CommercialMode     bool `json:"commercial_mode"`
	LoggingToFile      bool `json:"logging_to_file"`
	UsageStatsEnabled  bool `json:"usage_stats_enabled"`
	LogMaxTotalSizeMB  int  `json:"log_max_total_size_mb"`
}

// Network covers §6. Durations are stored as Go duration strings
// ("30s", "1h") because json/v2 has no default representation for
// time.Duration; ParsedMaxRetryInterval / ParsedSessionAffinityTTL
// expose the parsed values to consumers.
type Network struct {
	ProxyURL              string `json:"proxy_url,omitempty"`
	RequestRetry          int    `json:"request_retry"`
	MaxRetryCreds         int    `json:"max_retry_creds"`
	MaxRetryIntervalRaw   string `json:"max_retry_interval"`
	RoutingStrategy       string `json:"routing_strategy"`
	SessionAffinityTTLRaw string `json:"session_affinity_ttl"`
	ForceModelPrefix      bool   `json:"force_model_prefix"`
	SessionSticky         bool   `json:"session_sticky"`
	// PreferredRegion is the static fallback used by prefer-region
	// routing when GeoIP is disabled or fails to resolve the client IP.
	// Empty = no preference; selector uses RoutingStrategy alone.
	PreferredRegion       string `json:"preferred_region,omitempty"`
}

// MaxRetryInterval parses MaxRetryIntervalRaw, falling back to 30s.
func (n Network) MaxRetryInterval() time.Duration {
	if n.MaxRetryIntervalRaw == "" {
		return 30 * time.Second
	}
	d, err := time.ParseDuration(n.MaxRetryIntervalRaw)
	if err != nil {
		return 30 * time.Second
	}
	return d
}

// SessionAffinityTTL parses SessionAffinityTTLRaw, falling back to 1h.
func (n Network) SessionAffinityTTL() time.Duration {
	if n.SessionAffinityTTLRaw == "" {
		return time.Hour
	}
	d, err := time.ParseDuration(n.SessionAffinityTTLRaw)
	if err != nil {
		return time.Hour
	}
	return d
}

// Streaming covers §8.
type Streaming struct {
	KeepaliveSeconds       int `json:"keepalive_seconds"`
	BootstrapRetries       int `json:"bootstrap_retries"`
	NonStreamingKeepaliveS int `json:"non_streaming_keepalive_seconds"`
}

// CurrentSchemaVersion bumps when we make a breaking change.
const CurrentSchemaVersion = 1

// Default returns a Settings populated with the same defaults the
// flag layer uses, so a fresh install behaves identically to before.
func Default() *Settings {
	return &Settings{
		SchemaVersion: CurrentSchemaVersion,
		RemoteManagement: RemoteManagement{
			AllowRemote:  false,
			DisablePanel: false,
			PanelRepoURL: "",
		},
		APIKeys: nil,
		System: System{
			Debug:             false,
			CommercialMode:    false,
			LoggingToFile:     false,
			UsageStatsEnabled: true,
			LogMaxTotalSizeMB: 0,
		},
		Network: Network{
			ProxyURL:              "",
			RequestRetry:          3,
			MaxRetryCreds:         0,
			MaxRetryIntervalRaw:   "30s",
			RoutingStrategy:       "round-robin",
			SessionAffinityTTLRaw: "1h",
			ForceModelPrefix:      false,
			SessionSticky:         true,
		},
		Streaming: Streaming{
			KeepaliveSeconds:       0,
			BootstrapRetries:       1,
			NonStreamingKeepaliveS: 0,
		},
	}
}

// Store is the goroutine-safe in-memory + on-disk holder.
type Store struct {
	path string
	mu   sync.RWMutex
	cur  *Settings
}

// New opens or creates a Store rooted at path. If the file does not
// exist, defaults are written. If parsing fails, an error is returned
// — callers should refuse to start in that case (don't silently nuke
// the user's config).
func New(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("settings: path required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("settings: ensure dir: %w", err)
	}
	s := &Store{path: path}
	if err := s.load(); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		// First run — write defaults.
		s.cur = Default()
		if err := s.persistLocked(); err != nil {
			return nil, fmt.Errorf("settings: write defaults: %w", err)
		}
	}
	return s, nil
}

// Get returns a deep copy of the current settings (safe to mutate).
func (s *Store) Get() *Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c := *s.cur
	c.APIKeys = append([]APIKey(nil), s.cur.APIKeys...)
	return &c
}

// Update atomically applies fn to the current settings, persists the
// result to disk, and on success returns the new value. fn must
// return non-nil; if it returns an error, the change is discarded.
func (s *Store) Update(fn func(*Settings) error) (*Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	working := *s.cur
	working.APIKeys = append([]APIKey(nil), s.cur.APIKeys...)
	if err := fn(&working); err != nil {
		return nil, err
	}
	working.SchemaVersion = CurrentSchemaVersion
	prev := s.cur
	s.cur = &working
	if err := s.persistLocked(); err != nil {
		s.cur = prev
		return nil, err
	}
	out := working
	return &out, nil
}

// Path returns the on-disk location.
func (s *Store) Path() string { return s.path }

// load reads + parses the file. Caller does not need to hold the lock
// because load is only invoked from constructors.
func (s *Store) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	var parsed Settings
	if err := json.Unmarshal(data, &parsed); err != nil {
		return fmt.Errorf("settings: parse %s: %w", s.path, err)
	}
	s.cur = &parsed
	return nil
}

// persistLocked writes s.cur to disk atomically (write-rename).
// Caller must hold s.mu (any flavor).
func (s *Store) persistLocked() error {
	out, err := json.Marshal(s.cur)
	if err != nil {
		return fmt.Errorf("settings: marshal: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return fmt.Errorf("settings: write tmp: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("settings: rename: %w", err)
	}
	return nil
}

// DefaultSettingsPath returns ~/.config/kirocc/settings.json.
func DefaultSettingsPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "kirocc", "settings.json")
}

// ApplyToEnv exports the optimization knobs to the process environment.
// Only fills env vars that are currently unset, so explicit `KIROCC_*=...`
// at startup time still wins over the persisted UI value. Returns the
// list of variables that were actually set (for logging).
func (o Optimizations) ApplyToEnv() []string {
	var set []string
	apply := func(key, val string) {
		if val == "" {
			return
		}
		if _, ok := os.LookupEnv(key); ok {
			return
		}
		_ = os.Setenv(key, val)
		set = append(set, key)
	}
	if o.ForceThinkingBudget > 0 {
		apply("KIROCC_FORCE_THINKING_BUDGET", fmt.Sprintf("%d", o.ForceThinkingBudget))
	}
	apply("KIROCC_EXPERIMENT_THINKING_PROMPT", o.ThinkingPromptMode)
	apply("KIROCC_EXPERIMENT_THINKING_TOOL_CONTINUE", o.ThinkingToolContinueMode)
	apply("KIROCC_EXPERIMENT_SYSTEM_MODE", o.SystemMode)
	apply("KIROCC_UPSTREAM_ORIGIN", o.UpstreamOrigin)
	apply("KIROCC_MODEL_MAPPINGS", o.ModelMappings)
	apply("KIROCC_AUDIT_LOG", o.AuditLog)
	return set
}
