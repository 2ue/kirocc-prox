// Package kiro implements the provider.Provider interface for the Kiro /
// Amazon Q CodeWhisperer upstream. It is a thin wrapper over the existing
// auth.RefreshTokens and quota.Fetcher implementations.
package kiro

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/niuma/kirocc-pro/internal/auth"
	"github.com/niuma/kirocc-pro/internal/pool"
	"github.com/niuma/kirocc-pro/internal/proxyhttp"
	"github.com/niuma/kirocc-pro/internal/quota"
)

// ID is the identifier used in the JSON pool file and routing config.
const ID = "kiro"

// New returns a *Provider wrapping the given HTTP client (used as the
// fallback for credentials without a per-account ProxyURL). Pass nil to
// use a 30s-timeout default.
func New(httpClient *http.Client) *Provider {
	return &Provider{
		http:    httpClient,
		fetcher: quota.NewKiroFetcher(httpClient),
		clients: proxyhttp.New(0),
	}
}

// Provider is the Kiro implementation of provider.Provider.
type Provider struct {
	http    *http.Client     // fallback client for empty cred.ProxyURL
	fetcher quota.Fetcher    // fallback quota fetcher
	clients *proxyhttp.Pool  // per-proxy-URL client cache
}

// clientFor returns the HTTP client this credential should use. Empty
// ProxyURL falls back to p.http (the default global client).
func (p *Provider) clientFor(cred *pool.Credential) *http.Client {
	cred.Mu.RLock()
	url := cred.ProxyURL
	cred.Mu.RUnlock()
	if url == "" {
		return p.http
	}
	c, err := p.clients.ClientFor(url)
	if err != nil {
		slog.Warn("kiro: per-account proxy client build failed, falling back to default",
			"cred", cred.ID, "proxy", url, "err", err)
		return p.http
	}
	return c
}

// ID implements provider.Provider.
func (p *Provider) ID() string { return ID }

// DisplayName implements provider.Provider.
func (p *Provider) DisplayName() string { return "Kiro / Amazon Q" }

// HandlesModel matches any model whose name starts with "claude" (the only
// model family Kiro currently exposes). The match is case-insensitive.
func (p *Provider) HandlesModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(model), "claude")
}

// RefreshToken refreshes the credential's OAuth token in place via the
// existing auth package logic. Routes through the cred's pinned proxy
// URL when set, else the default client.
func (p *Provider) RefreshToken(ctx context.Context, cred *pool.Credential) error {
	cred.Mu.RLock()
	snap := cred.Credentials
	cred.Mu.RUnlock()

	refreshed, err := auth.RefreshTokens(ctx, &snap, p.clientFor(cred))
	if err != nil {
		return err
	}

	cred.Mu.Lock()
	cred.Credentials = *refreshed
	cred.Mu.Unlock()
	return nil
}

// FetchQuota queries the Kiro getUsageLimits endpoint, routed through
// the credential's pinned proxy when set.
func (p *Provider) FetchQuota(ctx context.Context, cred *pool.Credential) (*pool.KiroQuotaSnapshot, error) {
	cred.Mu.RLock()
	token := cred.AccessToken
	arn := cred.ProfileARN
	region := cred.Region
	cred.Mu.RUnlock()
	// Build a per-call fetcher using the cred's HTTP client so the
	// outbound request to q.{region}.amazonaws.com goes via that proxy.
	return quota.NewKiroFetcher(p.clientFor(cred)).Fetch(ctx, token, arn, region)
}

// SupportsOAuth reports whether the OAuth browser flow is wired.
func (p *Provider) SupportsOAuth() bool { return true }

// StartOAuth, CompleteOAuth implementations live in oauth.go.
