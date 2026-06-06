package config

import (
	"encoding/json/v2"
	"fmt"
	"math"
	"os"
	"strconv"
	"time"

	"github.com/niuma/kirocc-pro/internal/logging"
	"github.com/niuma/kirocc-pro/internal/promptcache"
)

// DefaultOTelBodyLimit is the default max bytes of request body to capture in OTel spans.
const DefaultOTelBodyLimit = 32 * 1024

// [fork] Pool / admin / usage defaults (Milestone 2-A + 2-B).
const (
	DefaultPoolStrategy         = "round-robin"
	DefaultAdminHost            = "127.0.0.1"
	DefaultAdminPort            = 3457
	DefaultQuotaPollInterval    = 3 * time.Minute
	DefaultUsageMemCap          = 10000
	DefaultSessionAffinityTTL   = 30 * time.Minute
	DefaultPromptCacheReadRatio = 0.98
	DefaultPostgresDSN          = "postgres://kirocc:kirocc_dev_password@127.0.0.1:15432/kirocc_pro?sslmode=disable"
	DefaultRedisAddr            = "127.0.0.1:16379"
	DefaultRedisKeyPrefix       = "kirocc:dev:"
	DefaultRedisLeaseTTL        = 30 * time.Minute
)

// Config is the runtime configuration for kirocc.
type Config struct {
	Port          int
	Host          string
	APIKey        string
	Debug         bool
	OTel          bool
	OTelBodyLimit int
	LogFile       logging.LogFileConfig

	PoolStrategy       string // "round-robin" | "fill-first" | "least-used" | "least-inflight" | "weighted-least-inflight"
	SessionAffinityTTL time.Duration

	// [fork] Admin server (Milestone 2-B). Bound separately on AdminHost:AdminPort
	// so the operator-facing endpoints never share a listener with the proxy
	// itself. Disable via AdminEnabled=false. When AdminKey is non-empty, all
	// /admin/* paths require either a session cookie obtained via the login
	// form OR an `Authorization: Bearer <key>` header.
	AdminEnabled   bool
	AdminHost      string
	AdminPort      int
	AdminKey       string
	AdminPublicURL string // optional: externally-visible base URL (https://admin.example.com)
	AdminTLSCert   string // optional: path to TLS cert PEM (enables HTTPS)
	AdminTLSKey    string // optional: path to TLS key PEM (required with cert)

	UsageMemCap int

	// [fork] Quota poller (Milestone 2-B).
	QuotaPollInterval time.Duration

	// PromptCache controls local Anthropic prompt-cache usage simulation. It does
	// not force real upstream cache hits; it only fills cache usage fields when
	// the request carries cache_control and upstream reports no cache tokens.
	PromptCacheEnabled         bool
	PromptCacheTargetReadRatio float64
	PromptCacheReportsJSON     string
	PromptCacheReports         promptcache.ReportConfig

	// [fork] CodexProxy routes the Codex (OpenAI) provider's outbound
	// HTTP traffic — OAuth + token refresh + future Responses-API
	// calls — through this proxy URL. OpenAI geo-blocks many regions;
	// Kiro is not blocked, so we keep the proxy per-provider rather
	// than global. Format: "http://user:pass@host:port" or
	// "socks5://host:port". Empty = inherit default transport
	// (http.ProxyFromEnvironment honors HTTPS_PROXY).
	CodexProxy string

	// GeoIPMMDB is the on-disk path to a MaxMind GeoLite2-Country MMDB
	// file. Empty disables GeoIP-based region routing (the proxy still
	// honors settings.Network.PreferredRegion as a static fallback).
	// Get the free file at
	// https://dev.maxmind.com/geoip/geolite2-free-geolocation-data
	GeoIPMMDB string

	// PostgreSQL is the durable source of truth for accounts, settings,
	// usage history, quota snapshots and pricing. Redis is the runtime
	// coordination layer for in-flight reservations, cooldowns, affinity and
	// short-lived locks.
	PostgresDSN    string
	RedisAddr      string
	RedisPassword  string
	RedisDB        int
	RedisKeyPrefix string
	RedisLeaseTTL  time.Duration
}

// ApplyEnvOverrides mutates cfg using KIROCC_* environment variables.
func ApplyEnvOverrides(cfg *Config) error {
	for _, key := range []string{"KIROCC_DB_PATH", "KIROCC_CREDS_JSON", "KIROCC_USAGE_DB", "KIROCC_SETTINGS"} {
		if os.Getenv(key) != "" {
			return fmt.Errorf("%s is no longer supported; use PostgreSQL/Redis configuration instead", key)
		}
	}
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
	if err := applyInt("KIROCC_USAGE_MEM_CAP", &cfg.UsageMemCap); err != nil {
		return err
	}
	if err := applyDuration("KIROCC_QUOTA_POLL_INTERVAL", &cfg.QuotaPollInterval); err != nil {
		return err
	}
	if err := applyBool("KIROCC_PROMPT_CACHE", &cfg.PromptCacheEnabled); err != nil {
		return err
	}
	if err := applyFloat("KIROCC_PROMPT_CACHE_TARGET_READ_RATIO", &cfg.PromptCacheTargetReadRatio); err != nil {
		return err
	}
	applyString("KIROCC_PROMPT_CACHE_REPORTS", &cfg.PromptCacheReportsJSON)
	applyString("KIROCC_GEOIP_MMDB", &cfg.GeoIPMMDB)
	applyString("KIROCC_POSTGRES_DSN", &cfg.PostgresDSN)
	applyString("KIROCC_REDIS_ADDR", &cfg.RedisAddr)
	applyString("KIROCC_REDIS_PASSWORD", &cfg.RedisPassword)
	if err := applyInt("KIROCC_REDIS_DB", &cfg.RedisDB); err != nil {
		return err
	}
	applyString("KIROCC_REDIS_KEY_PREFIX", &cfg.RedisKeyPrefix)
	if err := applyDuration("KIROCC_REDIS_LEASE_TTL", &cfg.RedisLeaseTTL); err != nil {
		return err
	}
	return nil
}

// Validate checks that the config is internally consistent. Returns an error
// describing the first violation found. Called after flag parsing and env
// overrides, before the server starts.
func (c *Config) Validate() error {
	if c.PostgresDSN == "" {
		c.PostgresDSN = DefaultPostgresDSN
	}
	if c.RedisAddr == "" {
		c.RedisAddr = DefaultRedisAddr
	}
	if c.RedisKeyPrefix == "" {
		c.RedisKeyPrefix = DefaultRedisKeyPrefix
	}
	if c.RedisLeaseTTL == 0 {
		c.RedisLeaseTTL = DefaultRedisLeaseTTL
	}
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
	case "", "round-robin", "fill-first", "least-used", "least-inflight", "weighted-least-inflight":
	default:
		return fmt.Errorf("pool-strategy must be one of round-robin|fill-first|least-used|least-inflight|weighted-least-inflight, got %q", c.PoolStrategy)
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
	if !isFiniteRatio(c.PromptCacheTargetReadRatio) || c.PromptCacheTargetReadRatio < 0 || c.PromptCacheTargetReadRatio > 0.99 {
		return fmt.Errorf("prompt-cache-target-read-ratio must be in 0..0.99, got %g", c.PromptCacheTargetReadRatio)
	}
	if c.PromptCacheReportsJSON != "" {
		if _, err := ParsePromptCacheReports(c.PromptCacheReportsJSON); err != nil {
			return err
		}
	}
	if c.RedisDB < 0 {
		return fmt.Errorf("redis-db must be >= 0, got %d", c.RedisDB)
	}
	if c.RedisLeaseTTL <= 0 {
		return fmt.Errorf("redis-lease-ttl must be > 0, got %s", c.RedisLeaseTTL)
	}
	return nil
}

func ParsePromptCacheReports(raw string) (promptcache.ReportConfig, error) {
	var cfg promptcache.ReportConfig
	if raw == "" {
		return cfg, nil
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return cfg, fmt.Errorf("prompt-cache-reports must be valid JSON: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("prompt-cache-reports invalid: %w", err)
	}
	return cfg.Normalized(), nil
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

func applyFloat(key string, dst *float64) error {
	if v := os.Getenv(key); v != "" {
		n, err := strconv.ParseFloat(v, 64)
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

func isFiniteRatio(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}
