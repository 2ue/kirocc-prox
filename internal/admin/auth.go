package admin

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
)

const (
	cookieName   = "kirocc-admin"
	cookieMaxAge = 8 * 60 * 60 // 8 hours
	cookieSalt   = "kirocc-admin-v1:"
)

// sessionToken returns the cookie value associated with the given admin key.
// Deterministic across restarts (so re-login is unnecessary on bounce), but
// changes whenever the operator rotates the key.
func sessionToken(adminKey string) string {
	h := sha256.Sum256([]byte(cookieSalt + adminKey))
	return hex.EncodeToString(h[:])
}

// constantTimeEqual is a constant-time string compare.
func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// wantsHTML guesses whether the request came from a browser navigation. Used
// by the auth middleware to choose between 302 redirect (browser) and 401
// JSON (curl / Bearer client). A request that carries an Authorization
// Bearer header is treated as an API client regardless of Accept.
func wantsHTML(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		return false
	}
	a := r.Header.Get("Accept")
	if a == "" {
		return true
	}
	if strings.Contains(a, "text/html") {
		return true
	}
	return false
}

// isPublicPath returns true for paths that bypass the auth middleware. The
// login form, logout endpoint, and ALL static assets are public; assets
// (CSS/JS) carry no secrets and the login page must be able to reference
// the shared stylesheet without prior authentication.
func isPublicPath(p string) bool {
	switch p {
	case "/admin/login", "/admin/login/", "/admin/logout":
		return true
	}
	return strings.HasPrefix(p, "/admin/assets/")
}

// authMiddleware enforces admin key authentication on protected paths. When
// adminKey is empty, the middleware is a no-op pass-through (and CSRF is
// also skipped: open mode has no session to hijack, so cross-site forgery
// is irrelevant — the network ACL is the only gate).
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	if s.adminKey == "" {
		return next
	}
	expectedCookie := sessionToken(s.adminKey)
	authedNext := s.csrfMiddleware(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isPublicPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if s.isAuthenticated(r, expectedCookie) {
			authedNext.ServeHTTP(w, r)
			return
		}
		if wantsHTML(r) {
			http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="kirocc-admin"`)
		http.Error(w, "authentication required", http.StatusUnauthorized)
	})
}

// csrfMiddleware enforces CSRF protection on state-changing methods.
// The OAuth callback (GET) carries its own state-nonce protection; login /
// logout already enforce cookie origin via SameSite=Strict. All other
// POST/PUT/DELETE handlers require ONE of:
//   - Content-Type starting with "application/json" (cross-site form
//     submits cannot set this without a preflight)
//   - Authorization: Bearer <admin-key> (CLI clients)
//   - X-Requested-With: XMLHttpRequest (legacy AJAX convention)
//
// All three are blocked by browser CORS+SameSite from a third-party page,
// so any of them is sufficient defense.
func (s *Server) csrfMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		// Login form submission is exempt: it intentionally accepts
		// a same-site form POST with no JSON header.
		if r.URL.Path == "/admin/login" || r.URL.Path == "/admin/logout" {
			next.ServeHTTP(w, r)
			return
		}
		ct := r.Header.Get("Content-Type")
		if strings.HasPrefix(ct, "application/json") ||
			strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") ||
			strings.EqualFold(r.Header.Get("X-Requested-With"), "XMLHttpRequest") {
			next.ServeHTTP(w, r)
			return
		}
		http.Error(w, "CSRF: state-changing request must use application/json content-type, Bearer auth, or X-Requested-With header", http.StatusForbidden)
	})
}

// isAuthenticated reports whether the request carries valid credentials,
// either via session cookie or Authorization: Bearer header.
func (s *Server) isAuthenticated(r *http.Request, expectedCookie string) bool {
	if c, err := r.Cookie(cookieName); err == nil {
		if constantTimeEqual(c.Value, expectedCookie) {
			return true
		}
	}
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		if constantTimeEqual(strings.TrimPrefix(auth, "Bearer "), s.adminKey) {
			return true
		}
	}
	return false
}

// handleLoginPage serves the login form. If the user is already authenticated
// it redirects to /admin.
func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if s.adminKey == "" {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	if s.isAuthenticated(r, sessionToken(s.adminKey)) {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	serveAsset(w, r, "login.html")
}

// handleLoginSubmit validates the posted key and sets a session cookie.
func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if s.adminKey == "" {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if !constantTimeEqual(r.PostFormValue("key"), s.adminKey) {
		http.Redirect(w, r, "/admin/login?error=1", http.StatusSeeOther)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    sessionToken(s.adminKey),
		Path:     "/admin",
		MaxAge:   cookieMaxAge,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// handleLogout clears the session cookie and redirects to the login page.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/admin",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	if s.adminKey == "" {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}
