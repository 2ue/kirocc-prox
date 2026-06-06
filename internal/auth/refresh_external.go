// [fork] Exposes the OAuth refresh path as a standalone function so the
// PostgreSQL-backed account pool can refresh individual *Credentials.

package auth

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// RefreshTokens returns a refreshed *Credentials by calling Kiro's social-
// login refresh endpoint (AuthType "social") or the AWS SSO OIDC endpoint
// (any other AuthType, treated as IDC).
//
// Fields not returned by the upstream endpoint (Region / SSORegion /
// ClientID / ClientSecret / AuthType / a still-present ProfileARN) are
// carried over from creds. Caller is responsible for persisting the result.
//
// httpClient may be nil; a 30s-timeout client is used when nil.
func RefreshTokens(ctx context.Context, creds *Credentials, httpClient *http.Client) (*Credentials, error) {
	if creds == nil {
		return nil, fmt.Errorf("refresh: nil credentials")
	}
	if creds.RefreshToken == "" {
		return nil, fmt.Errorf("refresh: empty refresh_token")
	}
	if httpClient == nil {
		httpClient = newDefaultHTTPClient()
	}

	var (
		body     []byte
		endpoint string
		label    string
		err      error
	)
	if creds.AuthType == "social" {
		region := creds.Region
		if region == "" {
			region = "us-east-1"
		}
		endpoint = defaultSocialEndpoint(region)
		body, err = json.Marshal(map[string]string{"refreshToken": creds.RefreshToken})
		label = "social"
	} else {
		if creds.ClientID == "" || creds.ClientSecret == "" {
			return nil, fmt.Errorf("refresh: idc credentials missing clientId/clientSecret")
		}
		if creds.SSORegion == "" {
			return nil, fmt.Errorf("refresh: idc credentials missing sso region")
		}
		endpoint = defaultOIDCEndpoint(creds.SSORegion)
		body, err = json.Marshal(map[string]string{
			"grantType":    "refresh_token",
			"clientId":     creds.ClientID,
			"clientSecret": creds.ClientSecret,
			"refreshToken": creds.RefreshToken,
		})
		label = "oidc"
	}
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("post token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		slog.DebugContext(ctx, "refresh: error response",
			"label", label, "status", resp.StatusCode, "body", string(errBody))
		return nil, fmt.Errorf("%s endpoint returned %d", label, resp.StatusCode)
	}

	var result tokenResponse
	if err := json.UnmarshalRead(resp.Body, &result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if result.AccessToken == "" {
		return nil, fmt.Errorf("%s response: empty access token", label)
	}
	if result.ExpiresIn <= 0 {
		return nil, fmt.Errorf("%s response: invalid expiresIn %d", label, result.ExpiresIn)
	}

	out := &Credentials{
		AccessToken:  result.AccessToken,
		RefreshToken: coalesce(result.RefreshToken, creds.RefreshToken),
		ExpiresAt:    time.Now().Unix() + result.ExpiresIn,
		Region:       creds.Region,
		SSORegion:    creds.SSORegion,
		ClientID:     creds.ClientID,
		ClientSecret: creds.ClientSecret,
		AuthType:     creds.AuthType,
		ProfileARN:   coalesce(result.ProfileArn, creds.ProfileARN),
	}
	return out, nil
}
