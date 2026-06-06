package auth

// Credentials holds upstream OAuth credentials loaded from the PostgreSQL
// account store.
type Credentials struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64
	Region       string
	SSORegion    string
	ClientID     string
	ClientSecret string
	ProfileARN   string
	AuthType     string // "social" or "idc"
}
