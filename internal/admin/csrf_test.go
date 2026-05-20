package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// CSRF middleware kicks in only when AdminKey is set. Without admin-key,
// open-mode test servers should pass through plain POSTs unhindered (the
// existing handler tests rely on this).

func TestCSRF_JSONContentTypeAllowed(t *testing.T) {
	s := NewServer("127.0.0.1", 0, "k", "", newFakeScheduler(), &fakeAggregator{}, &fakeCache{})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	client := newRawClient()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/admin/accounts/abc/disable", nil)
	req.Header.Set("Authorization", "Bearer k")
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		t.Errorf("JSON content-type should pass CSRF gate, got 403")
	}
}

func TestCSRF_BearerAllowed(t *testing.T) {
	s := NewServer("127.0.0.1", 0, "k", "", newFakeScheduler(), &fakeAggregator{}, &fakeCache{})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	client := newRawClient()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/admin/accounts/abc/disable", nil)
	req.Header.Set("Authorization", "Bearer k")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		t.Errorf("Bearer auth should pass CSRF gate, got 403")
	}
}

func TestCSRF_BlocksPlainPost(t *testing.T) {
	s := NewServer("127.0.0.1", 0, "k", "", newFakeScheduler(), &fakeAggregator{}, &fakeCache{})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	client := newRawClient()

	// Authenticate via cookie, then POST without JSON or X-Requested-With.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/admin/accounts/abc/disable",
		strings.NewReader("foo=bar"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: cookieName, Value: sessionToken("k")})
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("form-encoded POST with cookie should be blocked, got %d", resp.StatusCode)
	}
}

func TestCSRF_GETUnaffected(t *testing.T) {
	s := NewServer("127.0.0.1", 0, "k", "", newFakeScheduler(), &fakeAggregator{}, &fakeCache{})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	client := newRawClient()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/admin/health", nil)
	req.Header.Set("Authorization", "Bearer k")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET should never hit CSRF gate, got %d", resp.StatusCode)
	}
}

func TestCCSwitch_Config(t *testing.T) {
	s := NewServer("127.0.0.1", 0, "k", "", newFakeScheduler(), &fakeAggregator{}, &fakeCache{})
	s.SetProxyConfig("http://127.0.0.1:9326", "my-proxy-key")
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	client := newRawClient()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/admin/config/cc-switch", nil)
	req.Header.Set("Authorization", "Bearer k")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body := decode[ccSwitchResp](t, resp.Body)
	if body.BaseURL != "http://127.0.0.1:9326" {
		t.Errorf("BaseURL = %q", body.BaseURL)
	}
	if body.APIKey != "my-proxy-key" {
		t.Errorf("APIKey not surfaced")
	}
	if len(body.Models) == 0 {
		t.Errorf("expected non-empty model list")
	}
}
