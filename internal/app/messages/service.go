package messages

import (
	"context"
	"net/http"
	"time"

	"github.com/niuma/kirocc-pro/internal/dashboard"
	"github.com/niuma/kirocc-pro/internal/kiroclient"
	"github.com/niuma/kirocc-pro/internal/pool"
	"github.com/niuma/kirocc-pro/internal/promptcache"
	"github.com/niuma/kirocc-pro/internal/provider"
	"github.com/niuma/kirocc-pro/internal/usage"
)

// Service owns message execution and token counting flows.
//
// [fork] Acquires credentials via pool.Conductor so requests use the
// PostgreSQL account pool and Redis-coordinated runtime state. Each request
// also publishes a usage.Record after the upstream call settles.
type Service struct {
	conductor      pool.Conductor
	scheduler      pool.Scheduler
	aggregator     usage.Aggregator
	client         kiroclient.Client
	captureEnabled bool
	collector      *dashboard.Collector
	registry       *provider.Registry // optional; used to refuse non-Kiro routes until Phase III.2
	promptCache    *promptcache.Tracker

	// apiKeyUsageRecorder bumps the matched dynamic API key's used_tokens
	// counter after every successful request, so the auth middleware can
	// reject keys whose QuotaLimit has been reached. nil = disabled.
	apiKeyUsageRecorder func(keyID string, tokens int64) error

	// regionHinter is called per request to derive the preferred upstream
	// region. nil = no region routing (pool falls back to plain strategy).
	regionHinter func(r *http.Request) string

	promptCacheReports        promptcache.ReportConfig
	promptCacheReportProvider func() promptcache.ReportConfig
}

// Option configures a Service.
type Option func(*Service)

// WithCapture enables recording of full upstream request/response bodies on
// failure for debugging. Defaults to disabled; callers should enable it only
// when debug logging is on.
func WithCapture(enabled bool) Option {
	return func(s *Service) { s.captureEnabled = enabled }
}

// WithCollector attaches a dashboard Collector for request metrics.
func WithCollector(c *dashboard.Collector) Option {
	return func(s *Service) { s.collector = c }
}

// WithProviderRegistry attaches the provider registry; the handler then
// refuses requests whose resolved provider doesn't have an Execute path
// implemented yet (currently only "kiro" does). Pass nil to skip the
// check (single-provider deployments).
func WithProviderRegistry(r *provider.Registry) Option {
	return func(s *Service) { s.registry = r }
}

// WithAPIKeyUsageRecorder wires the per-key token counter. fn is invoked
// (off the hot path, after the upstream call settles) once per successful
// request with the matched API key id and the total tokens consumed.
func WithAPIKeyUsageRecorder(fn func(keyID string, tokens int64) error) Option {
	return func(s *Service) { s.apiKeyUsageRecorder = fn }
}

// WithRegionHinter wires the per-request region resolution. fn is called
// once per /v1/messages call with the live *http.Request, and should
// return a preferred AWS-style region (e.g. "us-east-1") or "" for no
// preference. Typical wiring: GeoIP lookup on client IP → static
// settings.Network.PreferredRegion fallback.
func WithRegionHinter(fn func(r *http.Request) string) Option {
	return func(s *Service) { s.regionHinter = fn }
}

// WithPromptCacheOptions configures the local prompt-cache usage simulator.
func WithPromptCacheOptions(opts promptcache.Options) Option {
	return func(s *Service) {
		if opts.Enabled {
			s.promptCache = promptcache.NewTracker(opts)
			s.promptCacheReports = promptcache.LegacyReportConfig(opts)
		}
	}
}

// WithPromptCacheReports configures path/profile-driven local usage reporting.
func WithPromptCacheReports(cfg promptcache.ReportConfig) Option {
	return func(s *Service) {
		cfg = cfg.Normalized()
		if cfg.Empty() {
			return
		}
		s.promptCacheReports = cfg
		if s.promptCache == nil {
			s.promptCache = promptcache.NewTracker(promptcache.DefaultOptions())
		}
	}
}

// WithPromptCacheReportProvider configures an authoritative runtime source for
// path/profile reporting, typically PostgreSQL settings edited from the admin UI.
func WithPromptCacheReportProvider(fn func() promptcache.ReportConfig) Option {
	return func(s *Service) {
		if fn == nil {
			return
		}
		s.promptCacheReportProvider = fn
		if s.promptCache == nil {
			s.promptCache = promptcache.NewTracker(promptcache.DefaultOptions())
		}
	}
}

// New constructs a message service.
//
// conductor and scheduler are required. aggregator may be nil; when nil,
// per-request usage records are dropped silently.
func New(conductor pool.Conductor, scheduler pool.Scheduler, aggregator usage.Aggregator, client kiroclient.Client, opts ...Option) *Service {
	s := &Service{
		conductor:  conductor,
		scheduler:  scheduler,
		aggregator: aggregator,
		client:     client,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// RunPromptCacheJanitor periodically trims the optional local prompt-cache
// tracker. It is a no-op when prompt-cache reporting is not configured.
func (s *Service) RunPromptCacheJanitor(ctx context.Context, interval time.Duration) {
	if s == nil || s.promptCache == nil {
		return
	}
	s.promptCache.RunJanitor(ctx, interval)
}
