package messages

import (
	"net/http"

	"github.com/niuma/kirocc-pro/internal/dashboard"
	"github.com/niuma/kirocc-pro/internal/kiroclient"
	"github.com/niuma/kirocc-pro/internal/pool"
	"github.com/niuma/kirocc-pro/internal/provider"
	"github.com/niuma/kirocc-pro/internal/usage"
)

// Service owns message execution and token counting flows.
//
// [fork] Acquires credentials via pool.Conductor (rather than a direct
// TokenGetter), so the handler can transparently support both the
// single-account SQLite path and a multi-account JSON pool. Each request
// also publishes a usage.Record after the upstream call settles.
type Service struct {
	conductor      pool.Conductor
	scheduler      pool.Scheduler
	aggregator     usage.Aggregator
	client         kiroclient.Client
	captureEnabled bool
	collector      *dashboard.Collector
	registry       *provider.Registry // optional; used to refuse non-Kiro routes until Phase III.2

	// apiKeyUsageRecorder bumps the matched dynamic API key's used_tokens
	// counter after every successful request, so the auth middleware can
	// reject keys whose QuotaLimit has been reached. nil = disabled.
	apiKeyUsageRecorder func(keyID string, tokens int64) error

	// regionHinter is called per request to derive the preferred upstream
	// region. nil = no region routing (pool falls back to plain strategy).
	regionHinter func(r *http.Request) string
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

// New constructs a message service.
//
// conductor and scheduler are required (use pool.NewSingleAccount for the
// single-account fallback). aggregator may be nil; when nil, per-request
// usage records are dropped silently.
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
