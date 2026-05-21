// [fork] Kiro OAuth flow. Matches the cockpit-tools desktop-app pattern:
// pick a free port from a small candidate set the Kiro auth server
// whitelists, start a loopback listener, build the auth URL with the
// matching redirect_uri, and exchange the captured code for tokens.

package kiro

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/niuma/kirocc-pro/internal/auth"
	"github.com/niuma/kirocc-pro/internal/oauth"
	"github.com/niuma/kirocc-pro/internal/pool"
	"github.com/niuma/kirocc-pro/internal/provider"
)

func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

const (
	authPortalURL = "https://app.kiro.dev/signin"
	tokenEndpoint = "https://prod.us-east-1.auth.desktop.kiro.dev/oauth/token"
)

// CallbackPortCandidates are the loopback ports the Kiro auth server is
// known to accept (verbatim from cockpit-tools'
// CALLBACK_PORT_CANDIDATES). The loopback tries them in order and binds
// the first one that's free.
var CallbackPortCandidates = []int{
	3128, 4649, 6588, 8008, 9091,
	49153, 50153, 51153, 52153, 53153,
}

// StartOAuth picks a candidate loopback port, starts the listener, and
// builds the Kiro authorization URL. The loopback accepts ANY path
// because Kiro's server picks the callback path dynamically.
func (p *Provider) StartOAuth(ctx context.Context, extras map[string]string) (*provider.OAuthFlow, error) {
	lb, err := oauth.NewLoopback(ctx, CallbackPortCandidates, "")
	if err != nil {
		return nil, fmt.Errorf("kiro oauth: start loopback: %w", err)
	}
	verifier, err := oauth.NewVerifier()
	if err != nil {
		lb.Close()
		return nil, err
	}
	state, err := oauth.NewState()
	if err != nil {
		lb.Close()
		return nil, err
	}

	// Build the auth URL with EXPLICIT PARAMETER ORDER matching
	// cockpit-tools' Rust format!() call. url.Values.Encode() sorts
	// alphabetically, which produces a different byte-for-byte URL and
	// can fail strict server-side validators. cockpit-tools order:
	//   state → code_challenge → code_challenge_method → redirect_uri → redirect_from
	authURL := fmt.Sprintf(
		"%s?state=%s&code_challenge=%s&code_challenge_method=S256&redirect_uri=%s&redirect_from=KiroIDE",
		authPortalURL,
		url.QueryEscape(state),
		url.QueryEscape(oauth.Challenge(verifier)),
		url.QueryEscape(lb.RedirectURI()),
	)

	return &provider.OAuthFlow{
		AuthURL:  authURL,
		State:    state,
		Verifier: verifier,
		Loopback: lb,
	}, nil
}

// CompleteOAuth exchanges params["code"] + verifier for tokens at Kiro's
// token endpoint and returns a *pool.Credential.
//
// The redirect_uri sent at token-exchange time mirrors what cockpit-tools
// constructs: the loopback's base URL + the path the OAuth server chose
// (captured in r.URL.Path of the redirect) + the optional login_option
// query parameter. We approximate by using the loopback's base RedirectURI
// + whatever path the redirect actually carried, falling back to the
// initial RedirectURI on missing data.
func (p *Provider) CompleteOAuth(ctx context.Context, params url.Values, flow *provider.OAuthFlow, extras map[string]string) (*pool.Credential, error) {
	if errStr := params.Get("error"); errStr != "" {
		return nil, fmt.Errorf("kiro oauth: provider returned error %q (%s)", errStr, params.Get("error_description"))
	}
	code := params.Get("code")
	if code == "" {
		return nil, errors.New("kiro oauth: redirect missing code")
	}
	if params.Get("state") != flow.State {
		return nil, errors.New("kiro oauth: state mismatch")
	}

	// Kiro requires a DIFFERENT redirect_uri at token-exchange time vs
	// auth-time. cockpit-tools' build_token_exchange_redirect_uri:
	//
	//   format!("{}{}?login_option={}",
	//           base_callback_url.trim_end_matches('/'),
	//           callback_path,                          // r.URL.Path of redirect
	//           urlencoding::encode(login_option));     // lowercased value
	//
	// The path is the one Kiro's auth server CHOSE (dynamic, e.g.
	// "/oauth/kiro/idc/callback"). login_option is always appended,
	// even when empty.
	base := fmt.Sprintf("http://localhost:%d", flow.Loopback.Port())
	cbPath := flow.Loopback.CapturedPath()
	if cbPath == "" {
		cbPath = "/"
	} else if !strings.HasPrefix(cbPath, "/") {
		cbPath = "/" + cbPath
	}
	loginOption := strings.ToLower(strings.TrimSpace(params.Get("login_option")))
	redirectURI := base + cbPath + "?login_option=" + url.QueryEscape(loginOption)

	body, err := json.Marshal(map[string]string{
		"code":          code,
		"code_verifier": flow.Verifier,
		"redirect_uri":  redirectURI,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal token request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := p.http
	if client == nil {
		client = http.DefaultClient
	}
	// If the caller pinned a proxy for this account (set in the
	// "add account" form), the OAuth token exchange ALSO goes via that
	// proxy so the upstream sees the matching egress IP from day one.
	if proxyURL := strings.TrimSpace(extras["proxy_url"]); proxyURL != "" {
		if c, err := p.clients.ClientFor(proxyURL); err == nil {
			client = c
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("post token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	rawBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		// Verbose error: dump exactly what we sent so failures aren't a
		// black box. code is logged truncated (it's a one-time auth code,
		// but still leaks if the log file is shared).
		slog.WarnContext(ctx, "kiro oauth token exchange failed",
			"status", resp.StatusCode,
			"endpoint", tokenEndpoint,
			"redirect_uri", redirectURI,
			"login_option", loginOption,
			"captured_path", cbPath,
			"code_prefix", truncateForLog(code, 8),
			"code_verifier_len", len(flow.Verifier),
			"response_body", string(rawBody))
		return nil, fmt.Errorf("kiro oauth token endpoint returned %d: %s (redirect_uri=%q login_option=%q)",
			resp.StatusCode, string(rawBody), redirectURI, loginOption)
	}

	var top map[string]any
	if err := json.Unmarshal(rawBody, &top); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}
	// Unwrap optional {"data": {...}} envelope.
	if inner, ok := top["data"].(map[string]any); ok {
		top = inner
	}

	accessToken := pickString(top, "accessToken", "access_token", "token", "idToken", "id_token", "accessTokenJwt")
	refreshToken := pickString(top, "refreshToken", "refresh_token", "refreshTokenJwt")
	profileARN := pickString(top, "profileArn", "profile_arn", "arn")
	expiresIn := pickInt(top, "expiresIn", "expires_in")

	if accessToken == "" {
		return nil, errors.New("kiro oauth: empty access_token in response")
	}
	if refreshToken == "" {
		return nil, errors.New("kiro oauth: empty refresh_token in response")
	}

	region := strings.TrimSpace(extras["region"])
	if region == "" {
		region = regionFromARN(profileARN)
	}
	if region == "" {
		region = "us-east-1"
	}

	expiresAt := int64(0)
	if expiresIn > 0 {
		expiresAt = time.Now().Unix() + expiresIn
	}

	id := strings.TrimSpace(extras["id"])
	if id == "" {
		id = fmt.Sprintf("kiro-%s", randomHex(4))
	}
	label := strings.TrimSpace(extras["label"])
	if label == "" {
		label = "Kiro (OAuth)"
	}

	cred := &pool.Credential{
		ID:       id,
		Label:    label,
		Provider: ID,
		Priority: 100,
		ProxyURL: strings.TrimSpace(extras["proxy_url"]),
		Credentials: auth.Credentials{
			AccessToken:  accessToken,
			RefreshToken: refreshToken,
			ExpiresAt:    expiresAt,
			Region:       region,
			ProfileARN:   profileARN,
			AuthType:     "social",
		},
	}
	return cred, nil
}

// --- helpers ---------------------------------------------------------

func pickString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func pickInt(m map[string]any, keys ...string) int64 {
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		switch x := v.(type) {
		case float64:
			return int64(x)
		case int64:
			return x
		case int:
			return int64(x)
		case string:
			var n int64
			for _, r := range x {
				if r < '0' || r > '9' {
					return 0
				}
				n = n*10 + int64(r-'0')
			}
			if n > 0 {
				return n
			}
		}
	}
	return 0
}

func regionFromARN(arn string) string {
	parts := strings.Split(arn, ":")
	if len(parts) < 5 {
		return ""
	}
	return strings.TrimSpace(parts[3])
}

func randomHex(nBytes int) string {
	b := make([]byte, nBytes)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
