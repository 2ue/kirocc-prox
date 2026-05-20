// [fork] Codex OAuth flow. Matches CLIProxyAPI / codex_cli_rs exactly:
// fixed loopback redirect_uri http://localhost:1455/auth/callback with
// 96-byte PKCE verifier, plus the proprietary Codex-flow query params.

package codex

import (
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/niuma/kirocc-pro/internal/auth"
	"github.com/niuma/kirocc-pro/internal/oauth"
	"github.com/niuma/kirocc-pro/internal/pool"
	"github.com/niuma/kirocc-pro/internal/provider"
)

const (
	authPortalURL = "https://auth.openai.com/oauth/authorize"
	tokenEndpoint = "https://auth.openai.com/oauth/token"

	// Public client_id baked into every public ChatGPT desktop / codex
	// CLI binary; not a secret.
	clientID = "app_EMoamEEZ73f0CkXaXp7hrann"

	authScopes    = "openid email profile offline_access"
	refreshScopes = "openid profile email"

	// 96-byte PKCE verifier per codex_cli_rs (above the RFC minimum of
	// 32 bytes). Encoded as base64url-no-padding → 128 chars.
	pkceVerifierBytes = 96

	// OpenAI's authorize endpoint accepts ONLY this loopback URI for the
	// codex_cli client. Both port and path are fixed.
	callbackPort = 1455
	callbackPath = "/auth/callback"
)

// StartOAuth binds the fixed loopback port 1455, builds the OpenAI
// authorization URL, and returns the carrier flow.
func (p *Provider) StartOAuth(ctx context.Context, extras map[string]string) (*provider.OAuthFlow, error) {
	lb, err := oauth.NewLoopback(ctx, []int{callbackPort}, callbackPath)
	if err != nil {
		return nil, fmt.Errorf("codex oauth: bind loopback %d: %w (likely another process holds the port — close it and retry)", callbackPort, err)
	}
	verifier, err := oauth.NewVerifierN(pkceVerifierBytes)
	if err != nil {
		lb.Close()
		return nil, err
	}
	state, err := oauth.NewState()
	if err != nil {
		lb.Close()
		return nil, err
	}

	params := url.Values{}
	params.Set("client_id", clientID)
	params.Set("response_type", "code")
	params.Set("redirect_uri", lb.RedirectURI())
	params.Set("scope", authScopes)
	params.Set("state", state)
	params.Set("code_challenge", oauth.Challenge(verifier))
	params.Set("code_challenge_method", "S256")
	params.Set("prompt", "login")
	params.Set("id_token_add_organizations", "true")
	params.Set("codex_cli_simplified_flow", "true")
	authURL := authPortalURL + "?" + params.Encode()

	return &provider.OAuthFlow{
		AuthURL:  authURL,
		State:    state,
		Verifier: verifier,
		Loopback: lb,
	}, nil
}

// CompleteOAuth exchanges params["code"] + verifier for tokens. The
// id_token JWT is parsed to extract chatgpt_account_id which the
// executor (Phase III.2) sends as the Chatgpt-Account-Id header.
func (p *Provider) CompleteOAuth(ctx context.Context, params url.Values, flow *provider.OAuthFlow, extras map[string]string) (*pool.Credential, error) {
	if errStr := params.Get("error"); errStr != "" {
		return nil, fmt.Errorf("codex oauth: provider returned error %q (%s)", errStr, params.Get("error_description"))
	}
	code := params.Get("code")
	if code == "" {
		return nil, errors.New("codex oauth: redirect missing code")
	}
	if params.Get("state") != flow.State {
		return nil, errors.New("codex oauth: state mismatch")
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", clientID)
	form.Set("code", code)
	form.Set("redirect_uri", flow.Loopback.RedirectURI())
	form.Set("code_verifier", flow.Verifier)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := p.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("post token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("codex token endpoint returned %d: %s", resp.StatusCode, string(body))
	}

	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}

	accessToken := pickString(raw, "access_token", "accessToken")
	refreshToken := pickString(raw, "refresh_token", "refreshToken")
	idToken := pickString(raw, "id_token", "idToken")
	expiresIn := pickInt(raw, "expires_in", "expiresIn")

	if accessToken == "" {
		return nil, errors.New("codex oauth: empty access_token in response")
	}

	accountID, planType := decodeOpenAIAuthClaim(idToken)

	id := strings.TrimSpace(extras["id"])
	if id == "" {
		id = fmt.Sprintf("codex-%d", time.Now().UnixNano()%1_000_000_000)
	}
	label := strings.TrimSpace(extras["label"])
	if label == "" {
		if planType != "" {
			label = fmt.Sprintf("ChatGPT %s (OAuth)", strings.ToUpper(planType))
		} else {
			label = "ChatGPT (OAuth)"
		}
	}

	expiresAt := int64(0)
	if expiresIn > 0 {
		expiresAt = time.Now().Unix() + expiresIn
	}

	metadata := map[string]string{}
	if accountID != "" {
		metadata["chatgpt_account_id"] = accountID
	}
	if planType != "" {
		metadata["chatgpt_plan_type"] = planType
	}
	if idToken != "" {
		metadata["id_token"] = idToken
	}

	cred := &pool.Credential{
		ID:       id,
		Label:    label,
		Provider: ID,
		Priority: 100,
		Metadata: metadata,
		Credentials: auth.Credentials{
			AccessToken:  accessToken,
			RefreshToken: refreshToken,
			ExpiresAt:    expiresAt,
			AuthType:     "social",
		},
	}
	return cred, nil
}

// decodeOpenAIAuthClaim extracts chatgpt_account_id + chatgpt_plan_type
// from an id_token JWT's "https://api.openai.com/auth" claim. Returns
// ("", "") on any decode failure — non-fatal, the flow still succeeds.
func decodeOpenAIAuthClaim(idToken string) (accountID, planType string) {
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return "", ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		raw, err = base64.URLEncoding.DecodeString(parts[1] + strings.Repeat("=", (4-len(parts[1])%4)%4))
		if err != nil {
			return "", ""
		}
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		return "", ""
	}
	auth, _ := claims["https://api.openai.com/auth"].(map[string]any)
	if auth == nil {
		return "", ""
	}
	accountID = pickString(auth, "chatgpt_account_id", "chatgptAccountId")
	planType = pickString(auth, "chatgpt_plan_type", "chatgptPlanType")
	return accountID, planType
}

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
		}
	}
	return 0
}
