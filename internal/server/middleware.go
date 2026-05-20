package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/niuma/kirocc-pro/internal/authctx"
	"github.com/niuma/kirocc-pro/internal/dashboard"
	"github.com/niuma/kirocc-pro/internal/httpx"
	"github.com/niuma/kirocc-pro/internal/logging"
	"github.com/niuma/kirocc-pro/internal/settings"
	"github.com/niuma/kirocc-pro/internal/tracing"
)

func traceMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traceID := tracing.ExtractTraceID(r.Context())
		if traceID == "" {
			traceID = logging.NewTraceID()
		}
		ctx := logging.WithTraceID(r.Context(), traceID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// authMiddleware enforces API key authentication when configured.
//
// Two key sources are checked, in order:
//
//  1. The legacy single -api-key flag (s.apiKey). Kept for backwards
//     compatibility with existing deployments.
//  2. The dynamic multi-key store (s.apiKeyValidator), populated by
//     the admin UI via /admin/api-keys.
//
// If EITHER source has any keys configured, the proxy requires a
// Bearer token that matches one of them. If both sources are empty
// the proxy is unauthenticated (loopback-only deployments).
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		legacyEnabled := s.apiKey != ""
		dynamicEnabled := s.apiKeyValidator != nil
		if !legacyEnabled && !dynamicEnabled {
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/health" || dashboard.IsDashboardPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		authHeader := r.Header.Get("Authorization")
		token, ok := strings.CutPrefix(authHeader, "Bearer ")
		if !ok {
			httpx.WriteError(w, http.StatusUnauthorized, httpx.ErrTypeAuthentication, "invalid API key")
			return
		}
		if legacyEnabled && subtle.ConstantTimeCompare([]byte(token), []byte(s.apiKey)) == 1 {
			next.ServeHTTP(w, r)
			return
		}
		if dynamicEnabled {
			id, err := s.apiKeyValidator(token)
			switch {
			case err == nil:
				ctx := authctx.WithAPIKeyID(r.Context(), id)
				ctx = authctx.WithDeviceID(ctx, deviceFingerprint(r))
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			case errors.Is(err, settings.ErrAPIKeyExpired):
				httpx.WriteError(w, http.StatusUnauthorized, httpx.ErrTypeAuthentication, "API key expired")
				return
			case errors.Is(err, settings.ErrAPIKeyOverQuota):
				httpx.WriteError(w, http.StatusTooManyRequests, httpx.ErrTypeRateLimit, "API key over quota")
				return
			}
		}
		httpx.WriteError(w, http.StatusUnauthorized, httpx.ErrTypeAuthentication, "invalid API key")
	})
}

// corsMiddleware adds CORS headers for localhost origins.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if isLocalhostOrigin(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
			w.Header().Set("Vary", "Origin")
		}

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isLocalhostOrigin checks if the origin is a localhost URL using strict URL parsing.
func isLocalhostOrigin(origin string) bool {
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return false
	}
	// Reject origins with userinfo or path components that could be spoofed.
	if u.User != nil || (u.Path != "" && u.Path != "/") {
		return false
	}
	host := u.Hostname()
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// deviceFingerprint returns a short hash of (peer IP + User-Agent) that's
// stable per device but doesn't expose either. Returns the first 12 hex
// chars of SHA-256 — about 48 bits of entropy, enough to distinguish
// devices for "show me what clients used this key" purposes without
// being a tracking identifier.
func deviceFingerprint(r *http.Request) string {
	ip := clientIP(r)
	ua := r.Header.Get("User-Agent")
	if ip == "" && ua == "" {
		return ""
	}
	h := sha256.Sum256([]byte(ip + "\x00" + ua))
	return hex.EncodeToString(h[:6])
}

// clientIP returns the peer IP, preferring X-Forwarded-For when set
// (admin server is loopback-only so this is informational, not trusted).
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
