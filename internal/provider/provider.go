// Package provider abstracts the per-upstream concerns (model routing,
// OAuth token refresh, quota query) behind a single interface so the rest
// of kirocc-pro can grow from a Kiro-only proxy into a multi-provider
// gateway (Kiro + OpenAI Codex + Google Gemini + ...).
//
// In Phase I.2 the interface only covers refresh + quota; the upstream
// request/response translation remains Kiro-specific in internal/kiroclient
// and internal/app/messages. Phase III (Codex provider) is when the
// Execute path will be extracted into the same interface.
package provider

import (
	"context"
	"errors"
	"net/url"

	"github.com/niuma/kirocc-pro/internal/oauth"
	"github.com/niuma/kirocc-pro/internal/pool"
)

// ErrOAuthNotSupported is returned by Provider.StartOAuth /
// Provider.CompleteOAuth when the provider doesn't implement an OAuth
// browser flow (e.g. it only supports manual token paste).
var ErrOAuthNotSupported = errors.New("provider: OAuth not supported")

// Provider is the abstraction over a single upstream model service.
// Implementations must be safe for concurrent use.
type Provider interface {
	// ID returns the stable identifier used in routing and in the JSON
	// "provider" field on each credential. Lowercase, no spaces.
	ID() string

	// DisplayName returns a human-readable name for the admin UI.
	DisplayName() string

	// HandlesModel reports whether requests for the given Anthropic-form
	// model name should be routed to this provider. The first registered
	// provider whose HandlesModel returns true wins in Registry.RouteFor.
	HandlesModel(model string) bool

	// RefreshToken refreshes the credential's OAuth tokens in place. It
	// is invoked by pool.MultiProviderRefresher when ShouldRefresh
	// detects the access token is within RefreshSkew of expiry.
	//
	// Returning a nil error means cred.Credentials carries fresh tokens
	// and may be persisted to disk by the caller.
	RefreshToken(ctx context.Context, cred *pool.Credential) error

	// FetchQuota queries the provider's getUsageLimits / equivalent
	// endpoint and returns a snapshot. nil snapshot + nil error is not
	// a valid return: implementations MUST return either a snapshot or
	// a wrapped error.
	FetchQuota(ctx context.Context, cred *pool.Credential) (*pool.KiroQuotaSnapshot, error)

	// SupportsOAuth reports whether StartOAuth / CompleteOAuth are
	// implemented. Providers that only accept manual token paste should
	// return false (the admin UI hides the "OAuth 登录" button in that
	// case).
	SupportsOAuth() bool

	// StartOAuth picks a free loopback callback port (per the upstream
	// OAuth server's accepted set), starts a temporary HTTP listener on
	// it, and returns the authorization URL the browser should open.
	//
	// The returned Flow carries the loopback handle, PKCE verifier, and
	// state nonce. The caller is responsible for calling Flow.Loopback.
	// Close() after CompleteOAuth runs (or on error / timeout).
	StartOAuth(ctx context.Context, extras map[string]string) (*OAuthFlow, error)

	// CompleteOAuth is invoked after the loopback captures the redirect.
	// params is the query string of the captured request (carries `code`,
	// `state`, optional `error`, plus provider-specific extras like Kiro's
	// `login_option`). flow is the Flow returned by StartOAuth.
	//
	// Returns a freshly-built Credential ready for the caller (admin
	// handler) to register into the scheduler and persist to JSON.
	CompleteOAuth(ctx context.Context, params url.Values, flow *OAuthFlow, extras map[string]string) (*pool.Credential, error)
}

// OAuthFlow is the carrier returned by Provider.StartOAuth. It holds
// everything the watcher goroutine needs to complete the flow after the
// Loopback captures the callback.
type OAuthFlow struct {
	AuthURL  string          // open this in the user's browser
	State    string          // expected state nonce in the redirect
	Verifier string          // PKCE code_verifier (kept opaque to admin)
	Loopback *oauth.Loopback // listener to await + close
}
