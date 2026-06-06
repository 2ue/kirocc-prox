package pool

// CredentialFile is the JSON import shape accepted by the admin account
// importer. The schema mirrors cockpit-tools / cli-proxy-api exports so an
// existing export payload can be imported into the PostgreSQL account store.
//
// Example payload (top-level array):
//
//	[
//	  {
//	    "id": "kiro-alice-001",
//	    "label": "alice@example.com (Pro)",
//	    "priority": 100,
//	    "disabled": false,
//	    "disable_cooling": false,
//	    "kiro_auth_token_raw": {
//	      "accessToken": "<redacted>",
//	      "refreshToken": "<redacted>",
//	      "expiresAt": "2026-05-20T10:00:00Z",
//	      "profileArn": "arn:aws:codewhisperer:us-east-1:000000000000:profile/EXAMPLE",
//	      "authMethod": "Social",
//	      "region": "us-east-1"
//	    }
//	  }
//	]
type CredentialFile struct {
	ID             string `json:"id"`
	Label          string `json:"label,omitempty"`
	Provider       string `json:"provider,omitempty"` // "kiro"; v1 ignores
	Priority       int    `json:"priority,omitempty"`
	Disabled       bool   `json:"disabled,omitempty"`
	DisableCooling bool   `json:"disable_cooling,omitempty"`
	MaxInFlight    int    `json:"max_in_flight,omitempty"`
	// ProxyURL pins this account's outbound auth-plane traffic (token
	// refresh + getUsageLimits + OAuth) through the named proxy. Empty
	// = default transport (HTTPS_PROXY env or direct). Multiple accounts
	// may share the same URL — kirocc will reuse one HTTP client per
	// unique proxy URL. Format: "http://user:pass@host:port" or
	// "socks5://host:port".
	ProxyURL         string           `json:"proxy_url,omitempty"`
	KiroAuthTokenRaw KiroAuthTokenRaw `json:"kiro_auth_token_raw"`
}

// KiroAuthTokenRaw mirrors the cockpit-tools / cli-proxy-api token export
// shape. Field names use camelCase to match the upstream format.
type KiroAuthTokenRaw struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    string `json:"expiresAt"` // RFC 3339 / ISO 8601
	ProfileARN   string `json:"profileArn"`
	AuthMethod   string `json:"authMethod"` // "Social" | "IAM" | "IDC"
	Region       string `json:"region,omitempty"`
	SSORegion    string `json:"ssoRegion,omitempty"`
	ClientID     string `json:"clientId,omitempty"`
	ClientSecret string `json:"clientSecret,omitempty"`
}
