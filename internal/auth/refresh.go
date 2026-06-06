package auth

import (
	"fmt"
	"net/http"
	"time"
)

const tokenValidityBuffer = 5 * time.Minute

func newDefaultHTTPClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}

func defaultOIDCEndpoint(ssoRegion string) string {
	return fmt.Sprintf("https://oidc.%s.amazonaws.com/token", ssoRegion)
}

func defaultSocialEndpoint(region string) string {
	if region == "" {
		region = "us-east-1"
	}
	return fmt.Sprintf("https://prod.%s.auth.desktop.kiro.dev/refreshToken", region)
}

// isTokenValid reports whether the token expires more than tokenValidityBuffer from now.
func isTokenValid(expiresAt int64) bool {
	return time.Unix(expiresAt, 0).After(time.Now().Add(tokenValidityBuffer))
}

// tokenResponse holds the common fields from a token refresh response.
type tokenResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresIn    int64  `json:"expiresIn"`
	ProfileArn   string `json:"profileArn"` // social only
}
