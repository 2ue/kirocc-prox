package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/niuma/kirocc-pro/internal/logging"
)

// DefaultOTelBodyLimit is the default max bytes of request body to capture in OTel spans.
const DefaultOTelBodyLimit = 32 * 1024

// [fork] Pool / admin / usage defaults (Milestone 2-A + 2-B).
const (
	DefaultPoolStrategy       = "round-robin"
	DefaultAdminHost          = "127.0.0.1"
	DefaultAdminPort          = 3457
	DefaultQuotaPollInterval  = 60 * time.Second
	DefaultUsageMemCap        = 10000
	DefaultSessionAffinityTTL = 30 * time.Minute
)

// Config is the runtime configuration for kirocc.
type Config struct {
	Port          int
	Host          string
	DBPath        string
	APIKey        string
	Debug         bool
	OTel          bool
	OTelBodyLimit int
	LogFile       logging.LogFileConfig

	// [fork] Multi-account pool (Milestone 2-A). Empty CredsJSON keeps the
	// single-account SQLite path with automatic token refresh; setting it
	// activates the JSON-backed pool (tokens do NOT auto-refresh in this
	// mode — the user re-exports the JSON via cockpit-tools when needed).
	CredsJSON          string
	PoolStrategy       string // "round-robin" | "fill-first" | "least-used"
	SessionAffinityTTL time.Duration

	// [fork] Admin server (Milestone 2-B). Bound separately on AdminHost:AdminPort
	// so the operator-facing endpoints never share a listener with the proxy
	// itself. Disable via AdminEnabled=false. When AdminKey is non-empty, all
	// /admin/* paths require either a session cookie obtained via the login
	// form OR an `Authorization: Bearer <key>` header.
	AdminEnabled  bool
	AdminHost     string
	AdminPort     int
	AdminKey      string
	AdminPublicURL string // optional: externally-visible base URL (https://admin.example.com)
	AdminTLSCert   string // optional: path to TLS cert PEM (enables HTTPS)
	AdminTLSKey    string // optional: path to TLS key PEM (required with cert)

	// [fork] Usage accounting (Milestone 2-B). UsageDB="" disables SQLite
	// persistence (memory-only). UsageMemCap caps the in-memory ring.
	UsageDB     string
	UsageMemCap int

	// [fork] Quota poller (Milestone 2-B).
	QuotaPollInterval time.Duration

	// [fork] CodexProxy routes the Codex (OpenAI) provider's outbound
	// HTTP traffic — OAuth + token refresh + future Responses-API
	// calls — through this proxy URL. OpenAI geo-blocks many regions;
	// Kiro is not blocked, so we keep the proxy per-provider rather
	// than global. Format: "http://user:pass@host:port" or
	// "socks5://host:port". Empty = inherit default transport
	// (http.ProxyFromEnvironment honors HTTPS_PROXY).
	CodexProxy string

	// SettingsPath is the on-disk JSON file holding runtime-mutable
	// configuration (multi API key list, admin-edited preferences).
	// Empty + admin disabled = no settings store. Empty + admin
	// enabled = ~/.config/kirocc/settings.json.
	SettingsPath string

	// GeoIPMMDB is the on-disk path to a MaxMind GeoLite2-Country MMDB
	// file. Empty disables GeoIP-based region routing (the proxy still
	// honors settings.Network.PreferredRegion as a static fallback).
	// Get the free file at
	// https://dev.maxmind.com/geoip/geolite2-free-geolocation-data
	GeoIPMMDB string
}

// DefaultCredsJSONPath returns the conventional location of the multi-account
// credentials file. Empty string when the home directory cannot be resolved.
func DefaultCredsJSONPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "kirocc", "credentials.json")
}

// DefaultUsageDBPath returns the conventional location of the usage SQLite file.
// Empty string when the home directory cannot be resolved.
func DefaultUsageDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "kirocc", "usage.sqlite")
}

// DefaultDBPath returns the default kiro-cli SQLite database location.
func DefaultDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return DefaultDBPathFor(runtime.GOOS, home)
}

// DefaultDBPathFor returns the default database location for the given OS and home directory.
func DefaultDBPathFor(goos, home string) string {
	switch goos {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "kiro-cli", "data.sqlite3")
	default:
		return filepath.Join(home, ".local", "share", "kiro-cli", "data.sqlite3")
	}
}

// ApplyEnvOverrides mutates cfg using KIROCC_* environment variables.
func ApplyEnvOverrides(cfg *Config) error {
	applyString("KIROCC_DB_PATH", &cfg.DBPath)
	applyString("KIROCC_API_KEY", &cfg.APIKey)
	applyString("KIROCC_HOST", &cfg.Host)
	if err := applyInt("KIROCC_PORT", &cfg.Port); err != nil {
		return err
	}
	if err := applyBool("KIROCC_DEBUG", &cfg.Debug); err != nil {
		return err
	}
	if err := applyBool("KIROCC_OTEL", &cfg.OTel); err != nil {
		return err
	}
	if err := applyInt("KIROCC_OTEL_BODY_LIMIT", &cfg.OTelBodyLimit); err != nil {
		return err
	}
	applyString("KIROCC_LOG_FILE", &cfg.LogFile.Path)
	if err := applyInt("KIROCC_LOG_MAX_SIZE", &cfg.LogFile.MaxSize); err != nil {
		return err
	}
	if err := applyInt("KIROCC_LOG_MAX_BACKUPS", &cfg.LogFile.MaxBackups); err != nil {
		return err
	}
	if err := applyInt("KIROCC_LOG_MAX_AGE", &cfg.LogFile.MaxAge); err != nil {
		return err
	}
	if err := applyBool("KIROCC_LOG_COMPRESS", &cfg.LogFile.Compress); err != nil {
		return err
	}
	if err := applyBool("KIROCC_LOG_CONSOLE", &cfg.LogFile.Console); err != nil {
		return err
	}
	// [fork] Pool / admin / usage / quota env overrides (Milestone 2-A + 2-B).
	applyString("KIROCC_CREDS_JSON", &cfg.CredsJSON)
	applyString("KIROCC_POOL_STRATEGY", &cfg.PoolStrategy)
	if err := applyDuration("KIROCC_AFFINITY_TTL", &cfg.SessionAffinityTTL); err != nil {
		return err
	}
	if err := applyBool("KIROCC_ADMIN", &cfg.AdminEnabled); err != nil {
		return err
	}
	applyString("KIROCC_ADMIN_HOST", &cfg.AdminHost)
	if err := applyInt("KIROCC_ADMIN_PORT", &cfg.AdminPort); err != nil {
		return err
	}
	applyString("KIROCC_ADMIN_KEY", &cfg.AdminKey)
	applyString("KIROCC_ADMIN_PUBLIC_URL", &cfg.AdminPublicURL)
	applyString("KIROCC_ADMIN_TLS_CERT", &cfg.AdminTLSCert)
	applyString("KIROCC_ADMIN_TLS_KEY", &cfg.AdminTLSKey)
	applyString("KIROCC_USAGE_DB", &cfg.UsageDB)
	if err := applyInt("KIROCC_USAGE_MEM_CAP", &cfg.UsageMemCap); err != nil {
		return err
	}
	if err := applyDuration("KIROCC_QUOTA_POLL_INTERVAL", &cfg.QuotaPollInterval); err != nil {
		return err
	}
	applyString("KIROCC_SETTINGS", &cfg.SettingsPath)
	applyString("KIROCC_GEOIP_MMDB", &cfg.GeoIPMMDB)
	return nil
}

// Validate checks that the config is internally consistent. Returns an error
// describing the first violation found. Called after flag parsing and env
// overrides, before the server starts.
func (c *Config) Validate() error {
	if c.Host == "" {
		return fmt.Errorf("host must not be empty")
	}
	if c.Port <= 0 || c.Port > 65535 {
		return fmt.Errorf("port must be in 1..65535, got %d", c.Port)
	}
	if c.OTelBodyLimit < 0 {
		return fmt.Errorf("otel-body-limit must be >= 0, got %d", c.OTelBodyLimit)
	}
	// [fork] Pool / admin / usage validation.
	if c.AdminEnabled {
		if c.AdminHost == "" {
			return fmt.Errorf("admin-host must not be empty when admin is enabled")
		}
		if c.AdminPort <= 0 || c.AdminPort > 65535 {
			return fmt.Errorf("admin-port must be in 1..65535, got %d", c.AdminPort)
		}
		if c.AdminPort == c.Port && c.AdminHost == c.Host {
			return fmt.Errorf("admin-port (%d) must differ from proxy port (%d) when sharing host", c.AdminPort, c.Port)
		}
		if (c.AdminTLSCert == "") != (c.AdminTLSKey == "") {
			return fmt.Errorf("admin-tls-cert and admin-tls-key must be set together")
		}
	}
	switch c.PoolStrategy {
	case "", "round-robin", "fill-first", "least-used":
	default:
		return fmt.Errorf("pool-strategy must be one of round-robin|fill-first|least-used, got %q", c.PoolStrategy)
	}
	if c.UsageMemCap < 0 {
		return fmt.Errorf("usage-mem-cap must be >= 0, got %d", c.UsageMemCap)
	}
	if c.QuotaPollInterval < 0 {
		return fmt.Errorf("quota-poll-interval must be >= 0, got %s", c.QuotaPollInterval)
	}
	if c.SessionAffinityTTL < 0 {
		return fmt.Errorf("affinity-ttl must be >= 0, got %s", c.SessionAffinityTTL)
	}
	return nil
}

func applyString(key string, dst *string) {
	if v := os.Getenv(key); v != "" {
		*dst = v
	}
}

func applyInt(key string, dst *int) error {
	if v := os.Getenv(key); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("invalid %s=%q: %w", key, v, err)
		}
		*dst = n
	}
	return nil
}

func applyBool(key string, dst *bool) error {
	if v := os.Getenv(key); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("invalid %s=%q: %w", key, v, err)
		}
		*dst = b
	}
	return nil
}

// [fork] applyDuration supports KIROCC_*_INTERVAL / _TTL env overrides.
func applyDuration(key string, dst *time.Duration) error {
	if v := os.Getenv(key); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("invalid %s=%q: %w", key, v, err)
		}
		*dst = d
	}
	return nil
}
