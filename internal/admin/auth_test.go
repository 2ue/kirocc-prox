package admin

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const testAdminKey = "test-key-12345"

func newAuthedTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	s := NewServer("127.0.0.1", 0, testAdminKey, newFakeScheduler(), &fakeAggregator{}, &fakeCache{})
	return httptest.NewServer(s.Handler())
}

// Client that does NOT follow redirects, so we can inspect 303 hops directly.
func newRawClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func TestAuth_OpenModeNoKey(t *testing.T) {
	// Existing test server uses adminKey="" (open mode); verify /admin/health
	// is reachable without any cookie or header.
	ts, cleanup := newTestServer(t, newFakeScheduler(), &fakeAggregator{}, &fakeCache{})
	defer cleanup()

	resp, err := http.Get(ts.URL + "/admin/health")
	if err != nil {
		t.Fatalf("get health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 in open mode, got %d", resp.StatusCode)
	}
}

func TestAuth_BrowserRedirectsToLogin(t *testing.T) {
	ts := newAuthedTestServer(t)
	defer ts.Close()
	client := newRawClient()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/admin", nil)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("get /admin: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/admin/login" {
		t.Fatalf("expected redirect to /admin/login, got %q", loc)
	}
}

func TestAuth_APIReturns401WhenUnauthenticated(t *testing.T) {
	ts := newAuthedTestServer(t)
	defer ts.Close()
	client := newRawClient()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/admin/health", nil)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("get health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("WWW-Authenticate"), "Bearer") {
		t.Fatalf("expected WWW-Authenticate Bearer challenge, got %q", resp.Header.Get("WWW-Authenticate"))
	}
}

func TestAuth_BearerHeaderAccepted(t *testing.T) {
	ts := newAuthedTestServer(t)
	defer ts.Close()
	client := newRawClient()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/admin/health", nil)
	req.Header.Set("Authorization", "Bearer "+testAdminKey)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("get health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 with Bearer, got %d", resp.StatusCode)
	}
}

func TestAuth_BearerWrongKeyRejected(t *testing.T) {
	ts := newAuthedTestServer(t)
	defer ts.Close()
	client := newRawClient()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/admin/health", nil)
	req.Header.Set("Authorization", "Bearer not-the-key")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("get health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestAuth_LoginWrongKeyRedirectsWithError(t *testing.T) {
	ts := newAuthedTestServer(t)
	defer ts.Close()
	client := newRawClient()

	form := url.Values{"key": {"wrong"}}
	resp, err := client.PostForm(ts.URL+"/admin/login", form)
	if err != nil {
		t.Fatalf("post login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/admin/login?error=1" {
		t.Fatalf("expected redirect with ?error=1, got %q", loc)
	}
	// No cookie should be set on failure.
	for _, c := range resp.Cookies() {
		if c.Name == cookieName && c.Value != "" {
			t.Fatalf("expected no session cookie on failed login, got %q", c.Value)
		}
	}
}

func TestAuth_LoginRightKeySetsCookieAndRedirects(t *testing.T) {
	ts := newAuthedTestServer(t)
	defer ts.Close()
	client := newRawClient()

	form := url.Values{"key": {testAdminKey}}
	resp, err := client.PostForm(ts.URL+"/admin/login", form)
	if err != nil {
		t.Fatalf("post login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/admin" {
		t.Fatalf("expected redirect to /admin, got %q", loc)
	}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == cookieName {
			cookie = c
		}
	}
	if cookie == nil || cookie.Value == "" {
		t.Fatalf("expected session cookie to be set")
	}
	if cookie.Value != sessionToken(testAdminKey) {
		t.Fatalf("cookie value mismatch: got %q", cookie.Value)
	}
	if !cookie.HttpOnly {
		t.Fatalf("expected HttpOnly cookie")
	}

	// Reuse the cookie on a follow-up request.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/admin/health", nil)
	req.AddCookie(cookie)
	resp2, err := client.Do(req)
	if err != nil {
		t.Fatalf("get health w/ cookie: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 with cookie, got %d", resp2.StatusCode)
	}
}

func TestAuth_LogoutClearsCookie(t *testing.T) {
	ts := newAuthedTestServer(t)
	defer ts.Close()
	client := newRawClient()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/admin/logout", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("post logout: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/admin/login" {
		t.Fatalf("expected redirect to /admin/login, got %q", loc)
	}
	// Verify a cookie was emitted with MaxAge<0 (delete instruction).
	var seen bool
	for _, c := range resp.Cookies() {
		if c.Name == cookieName {
			seen = true
			if c.MaxAge >= 0 {
				t.Fatalf("expected MaxAge<0 (delete), got %d", c.MaxAge)
			}
		}
	}
	if !seen {
		t.Fatalf("expected logout to emit a Set-Cookie for %s", cookieName)
	}
}

func TestAuth_LoginPageAlreadyAuthedRedirects(t *testing.T) {
	ts := newAuthedTestServer(t)
	defer ts.Close()
	client := newRawClient()

	// Already-authenticated visitor hitting /admin/login should be bounced
	// straight to the dashboard.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/admin/login", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: sessionToken(testAdminKey)})
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("get /admin/login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/admin" {
		t.Fatalf("expected redirect to /admin, got %q", loc)
	}
}
