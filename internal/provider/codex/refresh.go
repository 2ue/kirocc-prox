package codex

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/niuma/kirocc-pro/internal/pool"
)

// RefreshToken renews the credential's access token by exchanging its
// refresh_token at the same OpenAI token endpoint. The refresh response
// may rotate the refresh token; we persist whichever is returned.
func (p *Provider) RefreshToken(ctx context.Context, cred *pool.Credential) error {
	cred.Mu.RLock()
	rt := cred.RefreshToken
	cred.Mu.RUnlock()
	if rt == "" {
		return errors.New("codex refresh: empty refresh_token")
	}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", clientID)
	form.Set("refresh_token", rt)
	form.Set("scope", refreshScopes)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("build refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := p.http.Do(req)
	if err != nil {
		return fmt.Errorf("post refresh: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("codex refresh endpoint returned %d: %s", resp.StatusCode, string(body))
	}

	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return fmt.Errorf("decode refresh response: %w", err)
	}
	newAccess := pickString(raw, "access_token", "accessToken")
	newRefresh := pickString(raw, "refresh_token", "refreshToken")
	newIDToken := pickString(raw, "id_token", "idToken")
	expiresIn := pickInt(raw, "expires_in", "expiresIn")
	if newAccess == "" {
		return errors.New("codex refresh: empty access_token in response")
	}

	// id_token may rotate; re-extract chatgpt_account_id so the executor
	// always has the current one for the Chatgpt-Account-Id header.
	var newAccountID, newPlan string
	if newIDToken != "" {
		newAccountID, newPlan = decodeOpenAIAuthClaim(newIDToken)
	}

	cred.Mu.Lock()
	cred.AccessToken = newAccess
	if newRefresh != "" {
		cred.RefreshToken = newRefresh
	}
	if expiresIn > 0 {
		cred.ExpiresAt = time.Now().Unix() + expiresIn
	}
	if cred.Metadata == nil {
		cred.Metadata = map[string]string{}
	}
	if newIDToken != "" {
		cred.Metadata["id_token"] = newIDToken
	}
	if newAccountID != "" {
		cred.Metadata["chatgpt_account_id"] = newAccountID
	}
	if newPlan != "" {
		cred.Metadata["chatgpt_plan_type"] = newPlan
	}
	cred.Mu.Unlock()
	return nil
}
