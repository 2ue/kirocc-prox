package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/niuma/kirocc-pro/internal/admin"
	"github.com/niuma/kirocc-pro/internal/config"
	"github.com/niuma/kirocc-pro/internal/dashboard"
	"github.com/niuma/kirocc-pro/internal/georegion"
	"github.com/niuma/kirocc-pro/internal/kiroclient"
	"github.com/niuma/kirocc-pro/internal/logging"
	"github.com/niuma/kirocc-pro/internal/oauth"
	"github.com/niuma/kirocc-pro/internal/pool"
	"github.com/niuma/kirocc-pro/internal/promptcache"
	"github.com/niuma/kirocc-pro/internal/provider"
	codexprovider "github.com/niuma/kirocc-pro/internal/provider/codex"
	kiroprovider "github.com/niuma/kirocc-pro/internal/provider/kiro"
	"github.com/niuma/kirocc-pro/internal/quota"
	"github.com/niuma/kirocc-pro/internal/server"
	"github.com/niuma/kirocc-pro/internal/settings"
	"github.com/niuma/kirocc-pro/internal/storage"
	"github.com/niuma/kirocc-pro/internal/tokencount"
	"github.com/niuma/kirocc-pro/internal/tracing"
	"github.com/niuma/kirocc-pro/internal/usage"
	"github.com/redis/go-redis/v9"
)

var gitSHA = "dev"

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
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
	promptCacheReportsExplicit := cfg.PromptCacheReportsJSON != ""
	if promptCacheReportsExplicit {
		reports, err := config.ParsePromptCacheReports(cfg.PromptCacheReportsJSON)
		if err != nil {
			return fmt.Errorf("config: %w", err)
		}
		cfg.PromptCacheReports = reports
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

	// [fork] Provider registry: every upstream model service registers
	// here (Kiro today, Codex/Gemini/... in later milestones).
	registry := provider.NewRegistry()
	registry.Register(kiroprovider.New(nil))
	codexClient, err := buildCodexHTTPClient(cfg.CodexProxy)
	if err != nil {
		return fmt.Errorf("codex proxy: %w", err)
	}
	registry.Register(codexprovider.New(codexClient))

	pgDB, err := storage.OpenPostgres(ctx, cfg.PostgresDSN)
	if err != nil {
		return err
	}
	defer func() { _ = pgDB.Close() }()
	accountStore := storage.NewPostgresAccountStore(pgDB)

	redisClient := redis.NewClient(&redis.Options{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPassword,
		DB:       cfg.RedisDB,
	})
	if err := redisClient.Ping(ctx).Err(); err != nil {
		_ = redisClient.Close()
		return fmt.Errorf("ping redis: %w", err)
	}
	defer func() { _ = redisClient.Close() }()
	runtimeStore := pool.NewRedisRuntimeStore(redisClient, cfg.RedisKeyPrefix)

	scheduler, conductor, refresher, err := buildPool(ctx, cfg, accountStore, runtimeStore, registry)
	if err != nil {
		return fmt.Errorf("pool: %w", err)
	}

	aggregator, err := buildAggregator(cfg, pgDB)
	if err != nil {
		return fmt.Errorf("usage: %w", err)
	}
	defer func() { _ = aggregator.Close() }()

	settingsStore, err := settings.NewPostgres(pgDB)
	if err != nil {
		return fmt.Errorf("settings: %w", err)
	}
	if set := settingsStore.Get().Optimizations.ApplyToEnv(); len(set) > 0 {
		slog.Info("optimizations applied from settings", "vars", set)
	}
	slog.Info("settings store ready", "path", settingsStore.Path())

	kiroClient := buildKiroClient(cfg)

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
	srv := buildServer(conductor, scheduler, aggregator, kiroClient, cfg, collector, dashHandler, registry, settingsStore, geoResolver, promptCacheReportsExplicit)

	janitorCtx, janitorCancel := context.WithCancel(ctx)
	defer janitorCancel()
	if c, ok := conductor.(*pool.DefaultConductor); ok {
		go c.RunAffinityJanitor(janitorCtx, time.Minute)
	}
	go srv.RunPromptCacheJanitor(janitorCtx, time.Minute)

	// [fork] Quota poller (Kiro getUsageLimits → scheduler snapshots).
	quotaCache := quota.NewCache(quota.NewKiroFetcher(nil), quota.DefaultCacheTTL)
	if cfg.QuotaPollInterval > 0 {
		pollerCtx, pollerCancel := context.WithCancel(ctx)
		defer pollerCancel()
		go quota.NewPoller(scheduler, quotaCache, cfg.QuotaPollInterval, quota.DefaultMaxConcurrent,
			quota.WithCredentialFetcher(registry),
			quota.WithRefresher(refresher),
		).Run(pollerCtx)
	} else {
		slog.Info("quota poller disabled", "interval", cfg.QuotaPollInterval)
	}

	// [fork] Admin server (separate listener on AdminHost:AdminPort).
	var adminCancel context.CancelFunc = func() {}
	if cfg.AdminEnabled {
		adminSrv := admin.NewServer(cfg.AdminHost, cfg.AdminPort, cfg.AdminKey, scheduler, aggregator, quotaCache)
		adminSrv.SetRegistry(registry)
		adminSrv.SetOAuthCache(oauth.NewStateCache(0))
		adminSrv.SetPublicBaseURL(cfg.AdminPublicURL)
		adminSrv.SetTLS(cfg.AdminTLSCert, cfg.AdminTLSKey)
		adminSrv.SetSettings(settingsStore)
		adminSrv.SetCredentialStore(accountStore)
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
	fs.StringVar(&cfg.PoolStrategy, "pool-strategy", config.DefaultPoolStrategy, "credential selection strategy: round-robin|fill-first|least-used|least-inflight|weighted-least-inflight")
	fs.DurationVar(&cfg.SessionAffinityTTL, "affinity-ttl", config.DefaultSessionAffinityTTL, "how long a session sticks to a credential after inactivity")
	fs.BoolVar(&cfg.AdminEnabled, "admin", true, "enable the admin HTTP server (multi-account / quota dashboard)")
	fs.StringVar(&cfg.AdminHost, "admin-host", config.DefaultAdminHost, "admin server bind host")
	fs.IntVar(&cfg.AdminPort, "admin-port", config.DefaultAdminPort, "admin server bind port")
	fs.StringVar(&cfg.AdminKey, "admin-key", "", "admin login key; empty = open (no auth, warn at startup)")
	fs.StringVar(&cfg.AdminPublicURL, "admin-public-url", "", "externally-visible URL of the admin server (used for OAuth redirect_uri); e.g. https://admin.example.com")
	fs.StringVar(&cfg.AdminTLSCert, "admin-tls-cert", "", "PEM-encoded TLS certificate path; enables HTTPS when paired with -admin-tls-key")
	fs.StringVar(&cfg.AdminTLSKey, "admin-tls-key", "", "PEM-encoded TLS key path")
	fs.IntVar(&cfg.UsageMemCap, "usage-mem-cap", config.DefaultUsageMemCap, "in-memory usage ring buffer capacity")
	fs.DurationVar(&cfg.QuotaPollInterval, "quota-poll-interval", config.DefaultQuotaPollInterval, "interval between automatic Kiro quota refreshes")
	fs.BoolVar(&cfg.PromptCacheEnabled, "prompt-cache", false, "enable local prompt-cache usage simulation when cache_control is present")
	fs.Float64Var(&cfg.PromptCacheTargetReadRatio, "prompt-cache-target-read-ratio", config.DefaultPromptCacheReadRatio, "target cache read ratio for local prompt-cache usage simulation (0..0.99)")
	fs.StringVar(&cfg.PromptCacheReportsJSON, "prompt-cache-reports", "", "JSON path/profile usage reporting config; overrides settings prompt_cache_reports")
	fs.StringVar(&cfg.CodexProxy, "codex-proxy", "", "outbound HTTP/SOCKS proxy for the Codex (OpenAI) provider, e.g. http://127.0.0.1:7890 — bypasses regional blocks without routing other providers through it")
	fs.StringVar(&cfg.GeoIPMMDB, "geoip-mmdb", "", "path to a MaxMind GeoLite2-Country MMDB file; enables client-IP→region routing for the credential pool. Empty = GeoIP off (falls back to settings.network.preferred_region)")
	fs.StringVar(&cfg.PostgresDSN, "postgres-dsn", config.DefaultPostgresDSN, "PostgreSQL DSN for durable accounts/settings/usage state")
	fs.StringVar(&cfg.RedisAddr, "redis-addr", config.DefaultRedisAddr, "Redis address for runtime scheduling state")
	fs.StringVar(&cfg.RedisPassword, "redis-password", "", "Redis password")
	fs.IntVar(&cfg.RedisDB, "redis-db", 0, "Redis database number")
	fs.StringVar(&cfg.RedisKeyPrefix, "redis-key-prefix", config.DefaultRedisKeyPrefix, "Redis key prefix for this kirocc deployment")
	fs.DurationVar(&cfg.RedisLeaseTTL, "redis-lease-ttl", config.DefaultRedisLeaseTTL, "TTL for in-flight Redis reservations")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if fs.NArg() > 0 {
		return cfg, fmt.Errorf("unexpected positional argument %q", fs.Arg(0))
	}
	return cfg, nil
}

func buildKiroClient(cfg config.Config) kiroclient.Client {
	clientOpts := []kiroclient.HTTPClientOption{
		kiroclient.WithTokenCounter(tokencount.CountBytes),
	}
	if cfg.OTel {
		clientOpts = append(clientOpts, kiroclient.WithOTel(cfg.OTelBodyLimit))
	}
	return kiroclient.NewHTTPClient(clientOpts...)
}

func buildServer(conductor pool.Conductor, scheduler pool.Scheduler, agg usage.Aggregator, client kiroclient.Client, cfg config.Config, collector *dashboard.Collector, dashHandler *dashboard.Handler, registry *provider.Registry, settingsStore *settings.Store, geoResolver *georegion.Resolver, promptCacheReportsExplicit bool) *server.Server {
	var opts []server.ServerOption
	if cfg.OTel {
		opts = append(opts, server.WithOTel(cfg.OTelBodyLimit))
	}
	if cfg.Debug {
		opts = append(opts, server.WithCapture(true))
	}
	if cfg.PromptCacheEnabled {
		opts = append(opts, server.WithPromptCacheOptions(promptcache.Options{
			Enabled:         true,
			TargetReadRatio: cfg.PromptCacheTargetReadRatio,
		}))
	}
	if !cfg.PromptCacheReports.Empty() {
		opts = append(opts, server.WithPromptCacheReports(cfg.PromptCacheReports))
	}
	if settingsStore != nil && !promptCacheReportsExplicit {
		opts = append(opts, server.WithPromptCacheReportProvider(func() promptcache.ReportConfig {
			return settingsStore.Get().PromptCacheReports.Normalized()
		}))
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
		opts = append(opts, server.WithAPIKeyValidatorEnabled(func() bool {
			return len(settingsStore.Get().APIKeys) > 0
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

func buildPool(ctx context.Context, cfg config.Config, accountStore pool.CredentialStore, runtimeStore pool.RuntimeStateStore, registry *provider.Registry) (pool.Scheduler, pool.Conductor, pool.Refresher, error) {
	creds, err := accountStore.Load(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load postgres accounts: %w", err)
	}
	s := pool.NewDefaultScheduler()
	s.SetRuntimeState(runtimeStore)
	s.SetCredentialStore(accountStore)
	s.Register(creds)
	sel := pool.NewSelector(cfg.PoolStrategy)
	aff := pool.NewAffinity(cfg.SessionAffinityTTL)
	c := pool.NewConductor(s, sel, aff)
	c.SetRuntimeState(runtimeStore, cfg.RedisLeaseTTL)

	refresher := provider.NewMultiRefresherWithPersister(registry, accountStore.SaveOne)
	c.SetRefresher(refresher)

	slog.Info("pool: postgres+redis mode",
		"count", len(creds),
		"strategy", cfg.PoolStrategy,
		"providers", registry.Len(),
	)
	return s, c, refresher, nil
}

// buildAggregator wires the in-memory ring with the PostgreSQL append log.
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

func buildAggregator(cfg config.Config, pgDB *sql.DB) (usage.Aggregator, error) {
	mem := usage.NewMemoryStore(cfg.UsageMemCap)
	pgStore, err := usage.NewPostgresStore(pgDB)
	if err != nil {
		return nil, err
	}
	return usage.NewAggregator(mem, pgStore), nil
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
