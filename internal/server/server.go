package server

import (
	"context"
	"net/http"
	"strings"
	"time"

	messagesapp "github.com/niuma/kirocc-pro/internal/app/messages"
	"github.com/niuma/kirocc-pro/internal/dashboard"
	"github.com/niuma/kirocc-pro/internal/kiroclient"
	"github.com/niuma/kirocc-pro/internal/pool"
	"github.com/niuma/kirocc-pro/internal/promptcache"
	"github.com/niuma/kirocc-pro/internal/provider"
	"github.com/niuma/kirocc-pro/internal/tracing"
	"github.com/niuma/kirocc-pro/internal/usage"
)

// ServerOption configures a Server.
type ServerOption func(*Server)

// WithOTel enables OpenTelemetry tracing middleware.
func WithOTel(bodyLimit int) ServerOption {
	return func(s *Server) {
		s.otel = true
		s.otelBodyLimit = bodyLimit
	}
}

// WithCapture enables upstream capture logging in the messages service.
func WithCapture(enabled bool) ServerOption {
	return func(s *Server) { s.captureEnabled = enabled }
}

// WithDashboard attaches a dashboard handler and collector to the server.
func WithDashboard(h *dashboard.Handler, c *dashboard.Collector) ServerOption {
	return func(s *Server) {
		s.dashboardHandler = h
		s.collector = c
	}
}

// APIKeyValidator is the dynamic-multi-key auth callback. Implementations
// return the id of the matched key and nil on success; on rejection they
// return a non-nil error. The middleware maps the error to an HTTP
// status (settings.ErrAPIKeyExpired → 401 with reason, etc).
type APIKeyValidator func(token string) (id string, err error)

// Server is the HTTP server for the kirocc proxy.
type Server struct {
	apiKey              string
	apiKeyValidator     APIKeyValidator                        // optional; checks dynamic multi-key store
	apiKeyValidatorOn   func() bool                            // optional; reports whether the dynamic store has configured keys
	apiKeyUsageRecorder func(keyID string, tokens int64) error // optional; bumps used_tokens after each call
	regionHinter        func(r *http.Request) string           // optional; resolves preferred region per request
	otel                bool
	otelBodyLimit       int
	captureEnabled      bool
	mux                 *http.ServeMux
	messages            *messagesapp.Service
	dashboardHandler    *dashboard.Handler
	collector           *dashboard.Collector
	aggregator          usage.Aggregator
	registry            *provider.Registry
	promptCacheOptions  *promptcache.Options
	promptCacheReports  *promptcache.ReportConfig
	promptCacheProvider func() promptcache.ReportConfig
}

// WithProviderRegistry attaches a provider registry that the messages
// handler can use to refuse requests for not-yet-implemented providers.
func WithProviderRegistry(r *provider.Registry) ServerOption {
	return func(s *Server) { s.registry = r }
}

// WithAPIKeyValidator wires a runtime callback consulted alongside the
// legacy single -api-key. The callback returns the matched key id on
// success or a non-nil error on rejection (which the middleware maps to
// HTTP 401 / 429 / etc).
func WithAPIKeyValidator(fn APIKeyValidator) ServerOption {
	return func(s *Server) { s.apiKeyValidator = fn }
}

// WithAPIKeyValidatorEnabled controls whether the dynamic validator is active.
// When unset, wiring a validator means auth is enforced. Runtime stores should
// return true only when at least one dynamic API key is configured.
func WithAPIKeyValidatorEnabled(fn func() bool) ServerOption {
	return func(s *Server) { s.apiKeyValidatorOn = fn }
}

// WithAPIKeyUsageRecorder wires the per-key token counter used by the
// quota-limit enforcement loop. fn(keyID, tokens) is invoked after every
// successful request whose Bearer token matched a dynamic API key.
func WithAPIKeyUsageRecorder(fn func(keyID string, tokens int64) error) ServerOption {
	return func(s *Server) { s.apiKeyUsageRecorder = fn }
}

// WithRegionHinter forwards a per-request preferred-region resolver to
// the messages service so the pool can prefer credentials in the
// matched region.
func WithRegionHinter(fn func(r *http.Request) string) ServerOption {
	return func(s *Server) { s.regionHinter = fn }
}

// WithPromptCacheOptions configures local prompt-cache usage simulation.
func WithPromptCacheOptions(opts promptcache.Options) ServerOption {
	return func(s *Server) {
		if opts.Enabled {
			s.promptCacheOptions = &opts
		}
	}
}

// WithPromptCacheReports configures path/profile-driven local usage reporting.
func WithPromptCacheReports(cfg promptcache.ReportConfig) ServerOption {
	return func(s *Server) {
		cfg = cfg.Normalized()
		if !cfg.Empty() {
			s.promptCacheReports = &cfg
		}
	}
}

// WithPromptCacheReportProvider configures a runtime source for path/profile
// reporting, typically backed by PostgreSQL settings edited from the admin UI.
func WithPromptCacheReportProvider(fn func() promptcache.ReportConfig) ServerOption {
	return func(s *Server) {
		if fn != nil {
			s.promptCacheProvider = fn
		}
	}
}

// New creates a new Server.
//
// [fork] conductor + scheduler + aggregator replace the previous single
// TokenGetter parameter; main wires the PostgreSQL account pool and
// Redis-coordinated runtime state here.
func New(conductor pool.Conductor, scheduler pool.Scheduler, aggregator usage.Aggregator, apiKey string, client kiroclient.Client, opts ...ServerOption) *Server {
	s := &Server{
		apiKey:     apiKey,
		aggregator: aggregator,
		mux:        http.NewServeMux(),
	}
	for _, opt := range opts {
		opt(s)
	}
	msgOpts := []messagesapp.Option{messagesapp.WithCapture(s.captureEnabled)}
	if s.collector != nil {
		msgOpts = append(msgOpts, messagesapp.WithCollector(s.collector))
	}
	if s.registry != nil {
		msgOpts = append(msgOpts, messagesapp.WithProviderRegistry(s.registry))
	}
	if s.apiKeyUsageRecorder != nil {
		msgOpts = append(msgOpts, messagesapp.WithAPIKeyUsageRecorder(s.apiKeyUsageRecorder))
	}
	if s.regionHinter != nil {
		msgOpts = append(msgOpts, messagesapp.WithRegionHinter(s.regionHinter))
	}
	if s.promptCacheOptions != nil {
		msgOpts = append(msgOpts, messagesapp.WithPromptCacheOptions(*s.promptCacheOptions))
	}
	if s.promptCacheReports != nil {
		msgOpts = append(msgOpts, messagesapp.WithPromptCacheReports(*s.promptCacheReports))
	}
	if s.promptCacheProvider != nil {
		msgOpts = append(msgOpts, messagesapp.WithPromptCacheReportProvider(s.promptCacheProvider))
	}
	s.messages = messagesapp.New(conductor, scheduler, aggregator, client, msgOpts...)
	s.registerRoutes()
	return s
}

// Handler returns the http.Handler for the server.
func (s *Server) Handler() http.Handler {
	h := traceMiddleware(corsMiddleware(s.authMiddleware(s.mux)))
	if s.otel {
		h = tracing.Middleware(h, s.otelBodyLimit)
	}
	return h
}

// RunPromptCacheJanitor periodically trims local prompt-cache reporting state.
func (s *Server) RunPromptCacheJanitor(ctx context.Context, interval time.Duration) {
	if s == nil || s.messages == nil {
		return
	}
	s.messages.RunPromptCacheJanitor(ctx, interval)
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.mux.HandleFunc("GET /v1/models", s.handleModels)
	s.mux.HandleFunc("POST /v1/messages/count_tokens", s.messages.HandleCountTokens)
	s.mux.HandleFunc("POST /v1/messages", s.messages.HandleMessages)
	s.mux.HandleFunc("GET /api/", s.handleProfileAPI)
	s.mux.HandleFunc("POST /api/", s.handleProfileAPI)
	if s.dashboardHandler != nil {
		s.dashboardHandler.RegisterRoutes(s.mux)
	}
}

func (s *Server) handleProfileAPI(w http.ResponseWriter, r *http.Request) {
	path := normalizeProfileRequestPath(r.URL.Path)
	if !s.isConfiguredProfilePath(path) {
		http.NotFound(w, r)
		return
	}
	switch {
	case strings.HasSuffix(path, "/v1/models"):
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			methodNotAllowed(w, "GET, HEAD")
			return
		}
		s.handleModels(w, r)
	case strings.HasSuffix(path, "/v1/messages/count_tokens"):
		if r.Method != http.MethodPost {
			methodNotAllowed(w, "POST")
			return
		}
		s.messages.HandleCountTokens(w, r)
	case strings.HasSuffix(path, "/v1/messages"):
		if r.Method != http.MethodPost {
			methodNotAllowed(w, "POST")
			return
		}
		s.messages.HandleMessages(w, r)
	default:
		http.NotFound(w, r)
	}
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

func (s *Server) isConfiguredProfilePath(path string) bool {
	if !strings.HasPrefix(path, "/api/") {
		return false
	}
	cfg := s.currentPromptCacheReports()
	if cfg.Empty() {
		return false
	}
	matched, ok := cfg.Match(path)
	return ok && matched.Prefix != ""
}

func (s *Server) currentPromptCacheReports() promptcache.ReportConfig {
	if s.promptCacheProvider != nil {
		return s.promptCacheProvider().Normalized()
	}
	if s.promptCacheReports == nil {
		return promptcache.ReportConfig{}
	}
	return s.promptCacheReports.Normalized()
}

func normalizeProfileRequestPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	for strings.Contains(path, "//") {
		path = strings.ReplaceAll(path, "//", "/")
	}
	if len(path) > 1 {
		path = strings.TrimRight(path, "/")
	}
	return path
}
