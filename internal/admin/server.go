// Package admin exposes an HTTP server for the kirocc-pro local admin UI.
//
// The server binds explicitly to host:port (default 127.0.0.1:3457) and
// mounts a small REST API under /admin/* plus an embedded single-page
// dashboard at /admin. It is intentionally separate from the proxy server:
// the admin port should never be exposed publicly.
package admin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/niuma/kirocc-pro/internal/georegion"
	"github.com/niuma/kirocc-pro/internal/oauth"
	"github.com/niuma/kirocc-pro/internal/pool"
	"github.com/niuma/kirocc-pro/internal/provider"
	"github.com/niuma/kirocc-pro/internal/quota"
	"github.com/niuma/kirocc-pro/internal/settings"
	"github.com/niuma/kirocc-pro/internal/usage"
)

// DefaultHost is the loopback address the admin server binds to by default.
const DefaultHost = "127.0.0.1"

// DefaultPort is the TCP port the admin server listens on by default.
const DefaultPort = 3457

// Server is the admin HTTP server.
//
// It is safe to call Start once. Subsequent calls return an error.
type Server struct {
	host     string
	port     int
	adminKey string // empty = no auth required (open mode)

	sched         pool.Scheduler
	agg           usage.Aggregator
	cache         quota.Cache
	registry      *provider.Registry   // optional; nil = report empty provider list
	oauthCache    *oauth.StateCache    // optional; nil = OAuth endpoints return 503
	publicBaseURL string               // optional; overrides Host header in redirect_uri
	proxyBaseURL  string               // optional; advertised in /admin/config/cc-switch
	proxyAPIKey   string               // optional; surfaced in cc-switch config (masked elsewhere)
	tlsCert       string               // optional; PEM file path, enables HTTPS when paired with tlsKey
	tlsKey        string               // optional; PEM file path
	oauthFlows    *oauthFlowRegistry   // in-flight OAuth flows (state → entry)
	settings      *settings.Store      // optional; nil disables /admin/settings + /admin/api-keys
	refresher     pool.Refresher       // optional; lets manual refresh recover from expired access tokens
	geoResolver   *georegion.Resolver  // optional; surfaces GeoIP status in /admin/settings
	credStore     pool.CredentialStore // durable account store; PostgreSQL in production

	srv *http.Server
}

// NewServer constructs an admin server. Pass empty host / zero port to use
// the defaults. adminKey enables session-cookie + Bearer-header auth; pass
// "" to keep all /admin/* paths open (useful only on loopback). Account
// mutations require SetCredentialStore; the production store is PostgreSQL.
func NewServer(host string, port int, adminKey string, sched pool.Scheduler, agg usage.Aggregator, cache quota.Cache) *Server {
	if host == "" {
		host = DefaultHost
	}
	if port == 0 {
		port = DefaultPort
	}
	s := &Server{
		host:       host,
		port:       port,
		adminKey:   adminKey,
		sched:      sched,
		agg:        agg,
		cache:      cache,
		oauthFlows: newOAuthFlowRegistry(),
	}
	_ = s.registry // satisfy linter; set via WithRegistry
	s.srv = &http.Server{
		Addr:              net.JoinHostPort(host, strconv.Itoa(port)),
		Handler:           s.routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s
}

// Addr returns the bound address (host:port).
func (s *Server) Addr() string { return s.srv.Addr }

// SetRegistry attaches a provider.Registry so the /admin/providers endpoint
// can return the list of registered providers. Pass nil to remove.
func (s *Server) SetRegistry(r *provider.Registry) { s.registry = r }

// SetOAuthCache attaches a shared state cache used by /admin/oauth/start
// and /admin/oauth/callback. Pass nil to disable OAuth endpoints.
func (s *Server) SetOAuthCache(c *oauth.StateCache) { s.oauthCache = c }

// SetPublicBaseURL configures the externally-visible scheme://host[:port]
// used to build OAuth redirect URIs. Empty value falls back to the
// inbound request's Host header.
func (s *Server) SetPublicBaseURL(u string) { s.publicBaseURL = u }

// SetProxyConfig records the proxy port and API key so /admin/config/
// cc-switch can hand them to client tooling.
func (s *Server) SetProxyConfig(baseURL, apiKey string) {
	s.proxyBaseURL = baseURL
	s.proxyAPIKey = apiKey
}

// SetTLS enables HTTPS on the admin listener. Both cert and key must be
// readable PEM files. Pass empty strings to revert to plain HTTP.
func (s *Server) SetTLS(certPath, keyPath string) {
	s.tlsCert = certPath
	s.tlsKey = keyPath
}

// SetSettings attaches the runtime-mutable settings store. Pass nil to
// disable /admin/settings and /admin/api-keys endpoints.
func (s *Server) SetSettings(store *settings.Store) { s.settings = store }

func (s *Server) SetCredentialStore(store pool.CredentialStore) { s.credStore = store }

// SetRefresher attaches the pool refresher used by manual quota refresh to
// recover from expired OAuth access tokens. Pass nil to disable the
// pre-fetch refresh (the manual refresh will still attempt the upstream
// call with the cached access token).
func (s *Server) SetRefresher(r pool.Refresher) { s.refresher = r }

// SetGeoResolver lets the admin server surface MMDB load status in
// /admin/settings. Pass nil for no info (UI shows "GeoIP disabled").
func (s *Server) SetGeoResolver(r *georegion.Resolver) { s.geoResolver = r }

// Settings returns the attached store (may be nil).
func (s *Server) Settings() *settings.Store { return s.settings }

// Start binds the listener and serves until ctx is cancelled OR the server
// errors. Shutdown is invoked on ctx cancellation with a short grace period.
//
// If the server is bound to a non-loopback host on a privileged port (<1024)
// it refuses to start; otherwise non-loopback + privileged is permitted with
// a warning.
func (s *Server) Start(ctx context.Context) error {
	if s.host != DefaultHost && s.port < 1024 {
		return fmt.Errorf("admin: refusing to bind non-loopback host %q on privileged port %d", s.host, s.port)
	}
	if s.host != DefaultHost {
		slog.Warn("admin: binding to non-loopback host, ensure the port is firewalled",
			"host", s.host, "port", s.port)
	}
	if s.adminKey == "" {
		slog.Warn("admin: no admin-key set, all /admin/* endpoints are open (loopback only is safe; non-loopback is dangerous)")
	} else {
		slog.Info("admin: login key required", "auth", "cookie|bearer")
	}

	ln, err := net.Listen("tcp", s.srv.Addr)
	if err != nil {
		return fmt.Errorf("admin: listen %s: %w", s.srv.Addr, err)
	}
	scheme := "http"
	if s.tlsCert != "" && s.tlsKey != "" {
		scheme = "https"
	}
	slog.Info("admin: listening", "addr", scheme+"://"+ln.Addr().String())

	errCh := make(chan error, 1)
	go func() {
		var serveErr error
		if s.tlsCert != "" && s.tlsKey != "" {
			serveErr = s.srv.ServeTLS(ln, s.tlsCert, s.tlsKey)
		} else {
			serveErr = s.srv.Serve(ln)
		}
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			errCh <- serveErr
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Shutdown(shutCtx)
		<-errCh
		return nil
	case err := <-errCh:
		return err
	}
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.srv.Shutdown(ctx)
}

// Handler returns the HTTP handler (useful in tests via httptest.NewServer).
func (s *Server) Handler() http.Handler {
	return s.srv.Handler
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// Auth flow (always mounted; middleware skips these paths).
	mux.HandleFunc("GET /admin/login", s.handleLoginPage)
	mux.HandleFunc("POST /admin/login", s.handleLoginSubmit)
	mux.HandleFunc("POST /admin/logout", s.handleLogout)
	mux.HandleFunc("GET /admin/logout", s.handleLogout)

	// API.
	mux.HandleFunc("GET /admin/health", s.handleHealth)
	mux.HandleFunc("GET /admin/accounts", s.handleAccountsList)
	mux.HandleFunc("POST /admin/accounts", s.handleAccountCreate)
	mux.HandleFunc("POST /admin/accounts/import", s.handleAccountsImport)
	mux.HandleFunc("GET /admin/accounts/{id}", s.handleAccountGet)
	mux.HandleFunc("DELETE /admin/accounts/{id}", s.handleAccountDelete)
	mux.HandleFunc("PATCH /admin/accounts/{id}", s.handleAccountPatch)
	mux.HandleFunc("POST /admin/accounts/{id}/refresh", s.handleAccountRefresh)
	mux.HandleFunc("POST /admin/accounts/{id}/disable", s.handleAccountDisable)
	mux.HandleFunc("POST /admin/accounts/{id}/enable", s.handleAccountEnable)
	mux.HandleFunc("GET /admin/usage", s.handleUsage)
	mux.HandleFunc("GET /admin/usage/timeline", s.handleUsageTimeline)
	mux.HandleFunc("GET /admin/usage/recent", s.handleUsageRecent)
	mux.HandleFunc("GET /admin/providers", s.handleProviders)

	// OAuth flow + cc-switch config.
	mux.HandleFunc("POST /admin/oauth/start", s.handleOAuthStart)
	mux.HandleFunc("GET /admin/oauth/status", s.handleOAuthStatus)
	mux.HandleFunc("POST /admin/oauth/manual_callback", s.handleOAuthManualCallback)
	mux.HandleFunc("GET /admin/config/cc-switch", s.handleCCSwitchConfig)

	// Settings (runtime-mutable config) + multi API key list.
	mux.HandleFunc("GET /admin/optimizations", s.handleOptimizations)
	mux.HandleFunc("GET /admin/settings", s.handleSettingsGet)
	mux.HandleFunc("PUT /admin/settings", s.handleSettingsPut)
	mux.HandleFunc("GET /admin/api-keys", s.handleAPIKeysList)
	mux.HandleFunc("POST /admin/api-keys", s.handleAPIKeysCreate)
	mux.HandleFunc("PATCH /admin/api-keys/{id}", s.handleAPIKeyUpdate)
	mux.HandleFunc("POST /admin/api-keys/{id}/rotate", s.handleAPIKeyRotate)
	mux.HandleFunc("DELETE /admin/api-keys/{id}", s.handleAPIKeyDelete)

	// Removed in the PostgreSQL architecture; keep explicit tombstones so old
	// API clients don't get the SPA fallback and assume the file editor exists.
	mux.HandleFunc("GET /admin/credsfile", s.handleRemovedCredsFile)
	mux.HandleFunc("PUT /admin/credsfile", s.handleRemovedCredsFile)
	mux.HandleFunc("GET /admin/credsfile/download", s.handleRemovedCredsFile)

	// HTML / static.
	mux.HandleFunc("GET /admin", s.handleIndex)
	mux.HandleFunc("GET /admin/", s.handleIndex)
	mux.HandleFunc("GET /admin/assets/", s.handleAssets)

	return s.authMiddleware(mux)
}

func (s *Server) handleRemovedCredsFile(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "credentials file API removed; accounts are stored in PostgreSQL", http.StatusGone)
}
