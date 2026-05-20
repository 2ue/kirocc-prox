package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/niuma/kirocc-pro/internal/admin"
	"github.com/niuma/kirocc-pro/internal/auth"
	"github.com/niuma/kirocc-pro/internal/config"
	"github.com/niuma/kirocc-pro/internal/dashboard"
	"github.com/niuma/kirocc-pro/internal/georegion"
	"github.com/niuma/kirocc-pro/internal/kiroclient"
	"github.com/niuma/kirocc-pro/internal/logging"
	"github.com/niuma/kirocc-pro/internal/oauth"
	"github.com/niuma/kirocc-pro/internal/pool"
	"github.com/niuma/kirocc-pro/internal/provider"
	codexprovider "github.com/niuma/kirocc-pro/internal/provider/codex"
	kiroprovider "github.com/niuma/kirocc-pro/internal/provider/kiro"
	"github.com/niuma/kirocc-pro/internal/quota"
	"github.com/niuma/kirocc-pro/internal/server"
	"github.com/niuma/kirocc-pro/internal/settings"
	"github.com/niuma/kirocc-pro/internal/tokencount"
	"github.com/niuma/kirocc-pro/internal/tracing"
	"github.com/niuma/kirocc-pro/internal/usage"
)

var gitSHA = "dev"

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	// [fork] Subcommands run before flag parsing so `kirocc creds list` can
	// operate without starting the proxy.
	if len(args) > 0 && args[0] == "creds" {
		return runCredsCmd(args[1:])
	}
	cfg, err := parseFlags(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if err := config.ApplyEnvOverrides(&cfg); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}

	logHandler, logCloser := logging.NewHandler(cfg.Debug, cfg.LogFile)
	slog.SetDefault(slog.New(logHandler))
	if cfg.LogFile.Path != "" {
		slog.Info("file logging enabled", "path", cfg.LogFile.Path)
	}
	logStartupMetadata()

	var otelShutdown func(context.Context) error
	if cfg.OTel {
		shutdown, err := tracing.Init(ctx)
		if err != nil {
			return fmt.Errorf("otel init: %w", err)
		}
		otelShutdown = shutdown
		slog.Info("OpenTelemetry tracing enabled", "body_limit", cfg.OTelBodyLimit)
	}

	authMgr := auth.NewAuthManager(cfg.DBPath)

	// [fork] Provider registry: every upstream model service registers
	// here (Kiro today, Codex/Gemini/... in later milestones).
	registry := provider.NewRegistry()
	registry.Register(kiroprovider.New(nil))
	codexClient, err := buildCodexHTTPClient(cfg.CodexProxy)
	if err != nil {
		return fmt.Errorf("codex proxy: %w", err)
	}
	registry.Register(codexprovider.New(codexClient))

	// [fork] Pool: multi-account JSON if cfg.CredsJSON is set, else the
	// single-account SQLite adapter (which preserves the upstream
	// auto-refresh behavior).
	scheduler, conductor, refresher, err := buildPool(cfg, authMgr, registry)
	if err != nil {
		return fmt.Errorf("pool: %w", err)
	}

	// [fork] Usage aggregator (in-memory ring + optional SQLite append log).
	aggregator, err := buildAggregator(cfg)
	if err != nil {
		return fmt.Errorf("usage: %w", err)
	}
	defer func() { _ = aggregator.Close() }()

	// [fork] Settings store (admin-mutable runtime config). When the
	// path is set we keep a single instance shared across the proxy
	// (for multi-API-key auth) and the admin server (for editing).
	var settingsStore *settings.Store
	if path := resolveSettingsPath(cfg); path != "" {
		store, err := settings.New(path)
		if err != nil {
			return fmt.Errorf("settings: %w", err)
		}
		settingsStore = store
		// [fork] Push persisted KIROCC_* knobs into the process env so the
		// hot path (reqconv / thinking.go) sees them without a refactor.
		// Explicit env at launch always wins.
		if set := store.Get().Optimizations.ApplyToEnv(); len(set) > 0 {
			slog.Info("optimizations applied from settings", "vars", set)
		}
		slog.Info("settings store ready", "path", path)
	}

	kiroClient := buildKiroClient(authMgr, cfg)

	// [fork] GeoIP resolver: optional, off by default. When -geoip-mmdb
	// is provided we load the MaxMind file once and reuse a single
	// reader for the process lifetime. Failure to load is fatal — if
	// the operator wanted GeoIP routing, silently disabling it would
	// surprise them. Empty path = disabled resolver (no-op).
	geoResolver, err := georegion.New(cfg.GeoIPMMDB)
	if err != nil {
		return fmt.Errorf("geoip: %w", err)
	}
	if st := geoResolver.Status(); st.Loaded {
		slog.Info("geoip loaded", "path", st.Path, "db", st.DBType,
			"build_epoch", st.BuildEpoch, "nodes", st.Nodes)
	}
	defer func() { _ = geoResolver.Close() }()

	collector := dashboard.NewCollector(500)
	dashHandler := dashboard.NewHandler(collector, cfg)
	srv := buildServer(conductor, scheduler, aggregator, kiroClient, cfg, collector, dashHandler, registry, settingsStore, geoResolver)

	// [fork] Quota poller (Kiro getUsageLimits → scheduler snapshots).
	quotaCache := quota.NewCache(quota.NewKiroFetcher(nil), quota.DefaultCacheTTL)
	pollerCtx, pollerCancel := context.WithCancel(ctx)
	defer pollerCancel()
	go quota.NewPoller(scheduler, quotaCache, cfg.QuotaPollInterval, quota.DefaultMaxConcurrent).Run(pollerCtx)

	// [fork] Admin server (separate listener on AdminHost:AdminPort).
	var adminCancel context.CancelFunc = func() {}
	if cfg.AdminEnabled {
		adminSrv := admin.NewServer(cfg.AdminHost, cfg.AdminPort, cfg.AdminKey, cfg.CredsJSON, scheduler, aggregator, quotaCache)
		adminSrv.SetRegistry(registry)
		adminSrv.SetOAuthCache(oauth.NewStateCache(0))
		adminSrv.SetPublicBaseURL(cfg.AdminPublicURL)
		adminSrv.SetTLS(cfg.AdminTLSCert, cfg.AdminTLSKey)
		adminSrv.SetSettings(settingsStore)
		adminSrv.SetRefresher(refresher)
		adminSrv.SetGeoResolver(geoResolver)
		// proxy base url defaults to the configured proxy host/port (loopback ok)
		proxyScheme := "http"
		adminSrv.SetProxyConfig(fmt.Sprintf("%s://%s:%d", proxyScheme, cfg.Host, cfg.Port), cfg.APIKey)
		var adminCtx context.Context
		adminCtx, adminCancel = context.WithCancel(ctx)
		go func() {
			if err := adminSrv.Start(adminCtx); err != nil {
				slog.Error("admin server error", "err", err)
			}
		}()
		slog.Info("admin listening", "addr", "http://"+adminSrv.Addr())
	}
	defer adminCancel()

	// Eagerly initialize tiktoken so the first API request doesn't block on BPE data fetch.
	go tokencount.Preload()

	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	if cfg.APIKey == "" && !isLoopback(cfg.Host) {
		slog.Warn("server is binding to a non-loopback address without an API key — all endpoints are unauthenticated",
			"host", cfg.Host)
	}
	slog.Info("kirocc listening", "addr", "http://"+addr)
	slog.Info("set ANTHROPIC_BASE_URL to use with Claude Code", "url", "http://"+addr)
	slog.Info("dashboard available", "url", "http://"+addr+"/dashboard/")

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// WriteTimeout is intentionally not set: this server streams SSE responses
		// that can last minutes. A fixed WriteTimeout would kill long-running streams.
		// Slowloris is mitigated by ReadHeaderTimeout on the request side.
	}

	done := awaitShutdown(httpSrv, otelShutdown, logCloser)

	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("server: %w", err)
	}
	<-done
	return nil
}

func parseFlags(args []string) (config.Config, error) {
	fs := flag.NewFlagSet("kirocc", flag.ContinueOnError)
	var cfg config.Config
	fs.IntVar(&cfg.Port, "port", 9326, "listen port")
	fs.StringVar(&cfg.Host, "host", "127.0.0.1", "bind host")
	fs.StringVar(&cfg.DBPath, "db", config.DefaultDBPath(), "kiro-cli SQLite DB path")
	fs.StringVar(&cfg.APIKey, "api-key", "", "optional API key for authentication")
	fs.BoolVar(&cfg.Debug, "debug", false, "enable debug logging with OTel JSON Lines output")
	fs.BoolVar(&cfg.OTel, "otel", false, "enable OpenTelemetry tracing (OTLP HTTP exporter)")
	fs.IntVar(&cfg.OTelBodyLimit, "otel-body-limit", config.DefaultOTelBodyLimit, "max bytes of request body to capture in OTel spans (0 = unlimited)")
	fs.StringVar(&cfg.LogFile.Path, "log-file", "", "write logs to file with rotation (for agent debugging)")
	fs.IntVar(&cfg.LogFile.MaxSize, "log-max-size", logging.DefaultLogMaxSize, "max log file size in MB before rotation")
	fs.IntVar(&cfg.LogFile.MaxBackups, "log-max-backups", logging.DefaultLogMaxBackups, "max number of old log files to retain")
	fs.IntVar(&cfg.LogFile.MaxAge, "log-max-age", logging.DefaultLogMaxAge, "max days to retain old log files")
	fs.BoolVar(&cfg.LogFile.Compress, "log-compress", false, "compress rotated log files with gzip")
	fs.BoolVar(&cfg.LogFile.Console, "log-console", false, "also write logs to console when -log-file is set")
	// [fork] Pool / admin / usage / quota flags (Milestone 2-A + 2-B).
	fs.StringVar(&cfg.CredsJSON, "creds-json", "", "path to multi-account credentials JSON; empty = single-account SQLite mode (default ~/.config/kirocc/credentials.json)")
	fs.StringVar(&cfg.PoolStrategy, "pool-strategy", config.DefaultPoolStrategy, "credential selection strategy: round-robin|fill-first|least-used")
	fs.DurationVar(&cfg.SessionAffinityTTL, "affinity-ttl", config.DefaultSessionAffinityTTL, "how long a session sticks to a credential after inactivity")
	fs.BoolVar(&cfg.AdminEnabled, "admin", true, "enable the admin HTTP server (multi-account / quota dashboard)")
	fs.StringVar(&cfg.AdminHost, "admin-host", config.DefaultAdminHost, "admin server bind host")
	fs.IntVar(&cfg.AdminPort, "admin-port", config.DefaultAdminPort, "admin server bind port")
	fs.StringVar(&cfg.AdminKey, "admin-key", "", "admin login key; empty = open (no auth, warn at startup)")
	fs.StringVar(&cfg.AdminPublicURL, "admin-public-url", "", "externally-visible URL of the admin server (used for OAuth redirect_uri); e.g. https://admin.example.com")
	fs.StringVar(&cfg.AdminTLSCert, "admin-tls-cert", "", "PEM-encoded TLS certificate path; enables HTTPS when paired with -admin-tls-key")
	fs.StringVar(&cfg.AdminTLSKey, "admin-tls-key", "", "PEM-encoded TLS key path")
	fs.StringVar(&cfg.UsageDB, "usage-db", "", "SQLite path for usage persistence; empty = memory-only (default ~/.config/kirocc/usage.sqlite)")
	fs.IntVar(&cfg.UsageMemCap, "usage-mem-cap", config.DefaultUsageMemCap, "in-memory usage ring buffer capacity")
	fs.DurationVar(&cfg.QuotaPollInterval, "quota-poll-interval", config.DefaultQuotaPollInterval, "interval between automatic Kiro quota refreshes")
	fs.StringVar(&cfg.CodexProxy, "codex-proxy", "", "outbound HTTP/SOCKS proxy for the Codex (OpenAI) provider, e.g. http://127.0.0.1:7890 — bypasses regional blocks without routing other providers through it")
	fs.StringVar(&cfg.SettingsPath, "settings", "", "path to runtime settings JSON (defaults to ~/.config/kirocc/settings.json when admin is enabled); persists multi API key list and admin-mutable config")
	fs.StringVar(&cfg.GeoIPMMDB, "geoip-mmdb", "", "path to a MaxMind GeoLite2-Country MMDB file; enables client-IP→region routing for the credential pool. Empty = GeoIP off (falls back to settings.network.preferred_region)")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	// Apply defaults that need runtime resolution (home directory).
	if cfg.CredsJSON == "" {
		// Try the default path only if the file exists; otherwise leave empty
		// to keep single-account SQLite mode as the absolute default.
		if def := config.DefaultCredsJSONPath(); def != "" {
			if _, err := os.Stat(def); err == nil {
				cfg.CredsJSON = def
			}
		}
	}
	if cfg.UsageDB == "" {
		if def := config.DefaultUsageDBPath(); def != "" {
			cfg.UsageDB = def
		}
	}
	return cfg, nil
}

func buildKiroClient(authMgr *auth.AuthManager, cfg config.Config) kiroclient.Client {
	clientOpts := []kiroclient.HTTPClientOption{
		kiroclient.WithTokenCounter(tokencount.CountBytes),
	}
	// [fork] In single-account mode, hook the kiroclient's on-403 refresher to
	// the AuthManager so a stale token gets a one-shot retry. In
	// multi-account mode the same 403 surfaces to the handler, which marks
	// the credential and lets the next request pick a different one.
	if cfg.CredsJSON == "" {
		clientOpts = append(clientOpts, kiroclient.WithTokenRefresher(func(ctx context.Context) (string, error) {
			authMgr.InvalidateCache()
			creds, err := authMgr.GetToken(ctx)
			if err != nil {
				return "", err
			}
			return creds.AccessToken, nil
		}))
	}
	if cfg.OTel {
		clientOpts = append(clientOpts, kiroclient.WithOTel(cfg.OTelBodyLimit))
	}
	return kiroclient.NewHTTPClient(clientOpts...)
}

func buildServer(conductor pool.Conductor, scheduler pool.Scheduler, agg usage.Aggregator, client kiroclient.Client, cfg config.Config, collector *dashboard.Collector, dashHandler *dashboard.Handler, registry *provider.Registry, settingsStore *settings.Store, geoResolver *georegion.Resolver) *server.Server {
	var opts []server.ServerOption
	if cfg.OTel {
		opts = append(opts, server.WithOTel(cfg.OTelBodyLimit))
	}
	if cfg.Debug {
		opts = append(opts, server.WithCapture(true))
	}
	opts = append(opts, server.WithDashboard(dashHandler, collector))
	if registry != nil {
		opts = append(opts, server.WithProviderRegistry(registry))
	}
	if settingsStore != nil {
		opts = append(opts, server.WithAPIKeyValidator(func(supplied string) (string, error) {
			k, err := settingsStore.ValidateAPIKey(supplied)
			if err != nil {
				return k.ID, err
			}
			return k.ID, nil
		}))
		opts = append(opts, server.WithAPIKeyUsageRecorder(settingsStore.AddAPIKeyUsage))
	}
	if geoResolver != nil && (geoResolver.Enabled() || settingsStore != nil) {
		opts = append(opts, server.WithRegionHinter(func(r *http.Request) string {
			// 1. GeoIP lookup on client IP (only if a DB is loaded).
			if geoResolver.Enabled() {
				ip := proxyClientIP(r)
				if region := geoResolver.RegionForIPString(ip); region != "" {
					return region
				}
			}
			// 2. Settings-level static fallback.
			if settingsStore != nil {
				if pr := settingsStore.Get().Network.PreferredRegion; pr != "" {
					return pr
				}
			}
			return ""
		}))
	}
	return server.New(conductor, scheduler, agg, cfg.APIKey, client, opts...)
}

// resolveSettingsPath returns the on-disk path for the runtime settings
// store, or "" when settings persistence should be disabled.
//
// Priority:
//  1. cfg.SettingsPath (-settings flag / KIROCC_SETTINGS env var)
//  2. defaultSettingsPath() when admin is enabled (the admin UI is the
//     only consumer; running headless without admin → no need to keep
//     a settings file around)
func resolveSettingsPath(cfg config.Config) string {
	if cfg.SettingsPath != "" {
		return cfg.SettingsPath
	}
	if cfg.AdminEnabled {
		return settings.DefaultSettingsPath()
	}
	return ""
}

// [fork] buildPool returns a Scheduler + Conductor. When cfg.CredsJSON is
// set, it loads the multi-account file and constructs a DefaultScheduler +
// configured Selector + Conductor with session affinity. Otherwise it
// returns the single-account adapter wrapping authMgr. The registry is
// used by the multi-account refresher to dispatch RefreshToken per
// credential's Provider.
func buildPool(cfg config.Config, authMgr *auth.AuthManager, registry *provider.Registry) (pool.Scheduler, pool.Conductor, pool.Refresher, error) {
	if cfg.CredsJSON == "" {
		s, c, err := pool.NewSingleAccount(authMgr)
		if err != nil {
			return nil, nil, nil, err
		}
		slog.Info("pool: single-account mode (sqlite)")
		return s, c, nil, nil
	}
	creds, err := pool.LoadFromJSON(cfg.CredsJSON)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load creds %s: %w", cfg.CredsJSON, err)
	}
	s := pool.NewDefaultScheduler()
	s.Register(creds)
	sel := pool.NewSelector(cfg.PoolStrategy)
	aff := pool.NewAffinity(cfg.SessionAffinityTTL)
	c := pool.NewConductor(s, sel, aff)

	// [fork] Provider-aware refresher: routes RefreshToken via cred.Provider
	// to the matching provider.Provider in the registry. Persists rotated
	// tokens back to cfg.CredsJSON atomically.
	refresher := provider.NewMultiRefresher(registry, cfg.CredsJSON, s.All)
	c.SetRefresher(refresher)

	slog.Info("pool: multi-account mode",
		"path", cfg.CredsJSON,
		"count", len(creds),
		"strategy", cfg.PoolStrategy,
		"providers", registry.Len(),
	)
	return s, c, refresher, nil
}

// [fork] buildAggregator wires the in-memory ring with an optional SQLite
// append log. The SQLite path is created (with parent dirs) if missing.
// buildCodexHTTPClient returns an http.Client whose transport routes
// every request through proxyURL. Empty proxyURL means "use
// http.DefaultClient" (which falls back to HTTPS_PROXY env if set).
//
// Supported schemes: http, https, socks5.
func buildCodexHTTPClient(proxyURL string) (*http.Client, error) {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return nil, nil
	}
	u, err := neturl.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("parse proxy URL %q: %w", proxyURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" {
		return nil, fmt.Errorf("unsupported proxy scheme %q (use http/https/socks5)", u.Scheme)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyURL(u)
	slog.Info("codex provider: proxy enabled", "url", u.Redacted())
	return &http.Client{Transport: transport, Timeout: 60 * time.Second}, nil
}

func buildAggregator(cfg config.Config) (usage.Aggregator, error) {
	mem := usage.NewMemoryStore(cfg.UsageMemCap)
	var sql *usage.SQLiteStore
	if cfg.UsageDB != "" {
		if err := os.MkdirAll(filepath.Dir(cfg.UsageDB), 0o755); err != nil {
			return nil, fmt.Errorf("mkdir usage db dir: %w", err)
		}
		s, err := usage.NewSQLiteStore(cfg.UsageDB)
		if err != nil {
			return nil, fmt.Errorf("open usage db: %w", err)
		}
		sql = s
	}
	return usage.NewAggregator(mem, sql), nil
}

func logStartupMetadata() {
	binaryPath := currentBinaryPath()
	env := collectKiroEnv()
	prefixMode := thinkingPrefixMode()
	slog.Info("kirocc startup",
		"git_sha", gitSHA,
		"binary_path", binaryPath,
		"thinking_prefix_mode", prefixMode,
		"kiro_env", env,
	)
	kiroclient.AuditStartup(kiroclient.StartupAuditInfo{
		GitSHA:             gitSHA,
		BinaryPath:         binaryPath,
		ThinkingPrefixMode: prefixMode,
		Env:                env,
	})
}

func currentBinaryPath() string {
	path, err := os.Executable()
	if err != nil {
		return ""
	}
	return path
}

func thinkingPrefixMode() string {
	mode := os.Getenv("KIROCC_EXPERIMENT_THINKING_PROMPT")
	if mode == "" {
		return "default"
	}
	return mode
}

func collectKiroEnv() map[string]string {
	out := map[string]string{}
	for _, kv := range os.Environ() {
		key, value, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(key, "KIROCC_") {
			continue
		}
		out[key] = redactKiroEnvValue(key, value)
	}
	return out
}

func redactKiroEnvValue(key, value string) string {
	upper := strings.ToUpper(key)
	if strings.Contains(upper, "API_KEY") ||
		strings.Contains(upper, "TOKEN") ||
		strings.Contains(upper, "SECRET") ||
		strings.Contains(upper, "PASSWORD") {
		return "<redacted>"
	}
	return value
}

func isLoopback(host string) bool {
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}

// awaitShutdown registers a SIGINT/SIGTERM handler that gracefully stops the
// HTTP server, flushes OTel spans, and closes the log file. Returns a channel
// that closes when shutdown is complete.
func awaitShutdown(httpSrv *http.Server, otelShutdown func(context.Context) error, logCloser interface{ Close() error }) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigCh
		slog.Info("shutting down", "signal", sig.String())

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(ctx); err != nil {
			slog.Error("shutdown error", "err", err)
		}
		if otelShutdown != nil {
			if err := otelShutdown(ctx); err != nil {
				slog.Error("otel shutdown error", "err", err)
			}
		}
		if err := logCloser.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "log close error: %v\n", err)
		}
		close(done)
	}()
	return done
}

// proxyClientIP extracts the caller IP from a proxy /v1/messages
// request: prefers the first X-Forwarded-For hop (when the proxy itself
// is behind a reverse proxy), else r.RemoteAddr.
func proxyClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
