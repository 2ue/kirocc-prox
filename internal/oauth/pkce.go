// Package oauth provides reusable building blocks for OAuth 2.0 + PKCE
// flows: code-verifier / code-challenge generation, a TTL'd state cache
// for the cross-request data (verifier + nonce), and a cross-platform
// helper to launch the system browser pointed at an authorization URL.
//
// Provider-specific client IDs, scopes and endpoint URLs live in each
// provider package (e.g. internal/provider/kiro). This package stays
// vendor-neutral.
package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// PKCEVerifierLength is the byte length used to seed the verifier before
// base64url encoding. 32 bytes → 43 characters of base64url, which is the
// shortest length RFC 7636 allows for code_verifier.
const PKCEVerifierLength = 32

// NewVerifier returns a base64url-encoded (no padding) random verifier
// suitable for OAuth 2.0 PKCE per RFC 7636.
func NewVerifier() (string, error) {
	return NewVerifierN(PKCEVerifierLength)
}

// NewVerifierN allows providers that demand a larger verifier (Codex uses
// 96 random bytes → 128 base64url chars) to override the default size.
// nBytes must be in [16, 96] per RFC 7636.
func NewVerifierN(nBytes int) (string, error) {
	if nBytes < 16 || nBytes > 96 {
		return "", fmt.Errorf("oauth: verifier length %d outside [16,96]", nBytes)
	}
	buf := make([]byte, nBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("oauth: read random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// Challenge returns the S256 code challenge for a verifier.
func Challenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// NewState returns a base64url-encoded random nonce used as the OAuth
// `state` parameter. Different length and namespace from the verifier so
// the two are independent.
func NewState() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("oauth: read random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
