// Package codex implements the provider.Provider interface for OpenAI
// Codex (the ChatGPT Plus / Pro subscription tier's API). Phase III.1
// covers OAuth login, token refresh, and quota query. Phase III.2 will
// add request execution (Anthropic Messages API → OpenAI Responses API
// translation) once the Provider interface gains an Execute method.
package codex

import (
	"net/http"
	"strings"
)

// ID is the identifier used in the JSON pool file and routing.
const ID = "codex"

// SupportedModelPrefixes are the Anthropic-form prefixes that route to
// this provider. Kept tiny for now; extend when new models ship.
var SupportedModelPrefixes = []string{
	"gpt-",
	"chatgpt-",
	"o1",
	"o3",
	"o4",
	"codex-",
}

// New returns a Codex provider. Pass nil for httpClient to use a default.
func New(httpClient *http.Client) *Provider {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Provider{http: httpClient}
}

// Provider is the Codex implementation of provider.Provider.
type Provider struct {
	http *http.Client
}

// ID implements provider.Provider.
func (p *Provider) ID() string { return ID }

// DisplayName implements provider.Provider.
func (p *Provider) DisplayName() string { return "OpenAI Codex" }

// HandlesModel matches names beginning with any of SupportedModelPrefixes
// (case-insensitive).
func (p *Provider) HandlesModel(model string) bool {
	m := strings.ToLower(model)
	for _, pre := range SupportedModelPrefixes {
		if strings.HasPrefix(m, pre) {
			return true
		}
	}
	return false
}

// SupportsOAuth implements provider.Provider.
func (p *Provider) SupportsOAuth() bool { return true }

// StartOAuth / CompleteOAuth implementations live in oauth.go.
// RefreshToken / FetchQuota live in refresh.go / quota.go.
//
// Until those are written against the upstream specifics, the stub
// methods below return ErrOAuthPending so the handler can refuse the
// flow cleanly rather than silently 500ing.

// Compile-time interface check (kept commented since the import cycle
// would force a wider rearrangement just for this).
//
//   var _ provider.Provider = (*Provider)(nil)
