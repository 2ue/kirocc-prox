package pool

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/niuma/kirocc-pro/internal/auth"
)

func TestJSONFileRefresher_ShouldRefresh(t *testing.T) {
	now := time.Now().Unix()
	cases := []struct {
		name      string
		expiresAt int64
		refresh   string
		want      bool
	}{
		{"expired", now - 100, "rt", true},
		{"within skew", now + int64((RefreshSkew - time.Minute).Seconds()), "rt", true},
		{"fresh", now + 3600, "rt", false},
		{"no expiry", 0, "rt", false},
		{"no refresh token", now - 100, "", false},
	}
	r := NewJSONFileRefresher("", nil, nil)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Credential{ID: "x"}
			c.Credentials = auth.Credentials{
				RefreshToken: tc.refresh,
				ExpiresAt:    tc.expiresAt,
			}
			if got := r.ShouldRefresh(c); got != tc.want {
				t.Errorf("ShouldRefresh(%+v) = %v, want %v", tc, got, tc.want)
			}
		})
	}
}

// rewriteTransport routes any outbound request whose host suffix matches
// "amazonaws.com" or "kiro.dev" to the given httptest base URL. Used to
// exercise auth.RefreshTokens against a local test server without changing
// the production endpoint constants.
type rewriteTransport struct {
	base *url.URL
}

func (t *rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.Contains(r.URL.Host, "amazonaws.com") || strings.Contains(r.URL.Host, "kiro.dev") {
		r2 := *r
		u := *r.URL
		u.Scheme = t.base.Scheme
		u.Host = t.base.Host
		r2.URL = &u
		r2.Host = t.base.Host
		return http.DefaultTransport.RoundTrip(&r2)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func TestJSONFileRefresher_RefreshUpdatesInMemoryAndDisk(t *testing.T) {
	// Mock Kiro social refresh endpoint.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method %s", r.Method)
		}
		resp := map[string]any{
			"accessToken":  "new-access-token",
			"refreshToken": "new-refresh-token",
			"expiresIn":    3600,
			"profileArn":   "arn:aws:codewhisperer:us-east-1:000000000000:profile/EXAMPLE",
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.MarshalWrite(w, resp)
	}))
	defer ts.Close()

	base, _ := url.Parse(ts.URL)
	httpClient := &http.Client{Transport: &rewriteTransport{base: base}}

	// Seed a creds JSON file.
	dir := t.TempDir()
	credsPath := filepath.Join(dir, "creds.json")
	initial := []byte(`[
	  {
	    "id": "kiro-alice",
	    "label": "alice@example.com",
	    "priority": 100,
	    "kiro_auth_token_raw": {
	      "accessToken":  "old-token",
	      "refreshToken": "old-refresh",
	      "expiresAt":    "2000-01-01T00:00:00Z",
	      "profileArn":   "arn:aws:codewhisperer:us-east-1:000000000000:profile/EXAMPLE",
	      "authMethod":   "Social",
	      "region":       "us-east-1"
	    }
	  }
	]`)
	if err := os.WriteFile(credsPath, initial, 0o600); err != nil {
		t.Fatalf("seed creds: %v", err)
	}

	creds, err := LoadFromJSON(credsPath)
	if err != nil {
		t.Fatalf("LoadFromJSON: %v", err)
	}
	if len(creds) != 1 {
		t.Fatalf("expected 1 cred, got %d", len(creds))
	}
	cred := creds[0]

	r := NewJSONFileRefresher(credsPath, func() []*Credential { return creds }, httpClient)
	if !r.ShouldRefresh(cred) {
		t.Fatalf("expected ShouldRefresh to return true for expired token")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Refresh(ctx, cred); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	// In-memory cred updated.
	cred.Mu.RLock()
	if cred.AccessToken != "new-access-token" {
		t.Errorf("in-memory access token = %q, want new-access-token", cred.AccessToken)
	}
	if cred.RefreshToken != "new-refresh-token" {
		t.Errorf("in-memory refresh token = %q, want new-refresh-token", cred.RefreshToken)
	}
	if cred.ExpiresAt < time.Now().Unix()+3500 {
		t.Errorf("expected ExpiresAt ~now+3600, got %d", cred.ExpiresAt)
	}
	cred.Mu.RUnlock()

	// Disk file updated. Reload and verify token roundtrip.
	reloaded, err := LoadFromJSON(credsPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(reloaded) != 1 {
		t.Fatalf("reload count = %d", len(reloaded))
	}
	if reloaded[0].AccessToken != "new-access-token" {
		t.Errorf("disk access token = %q, want new-access-token", reloaded[0].AccessToken)
	}
	if reloaded[0].RefreshToken != "new-refresh-token" {
		t.Errorf("disk refresh token = %q, want new-refresh-token", reloaded[0].RefreshToken)
	}
}

// fakeRefresher records invocations for conductor wiring assertions.
type fakeRefresher struct {
	shouldRefresh bool
	refreshErr    error

	shouldCalls   int
	refreshCalls  int
	lastRefreshed string
}

func (f *fakeRefresher) ShouldRefresh(c *Credential) bool {
	f.shouldCalls++
	return f.shouldRefresh
}
func (f *fakeRefresher) Refresh(_ context.Context, c *Credential) error {
	f.refreshCalls++
	f.lastRefreshed = c.ID
	return f.refreshErr
}

func TestConductor_RefreshesStaleCredOnAcquire(t *testing.T) {
	s := NewDefaultScheduler()
	cred := &Credential{ID: "alice", Priority: 100}
	s.Register([]*Credential{cred})

	cond := NewConductor(s, &RoundRobinSelector{}, NewAffinity(time.Minute))
	fr := &fakeRefresher{shouldRefresh: true}
	cond.SetRefresher(fr)

	got, err := cond.Acquire(context.Background(), "claude-sonnet-4.6", "session-1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if got.ID != "alice" {
		t.Fatalf("got cred %q, want alice", got.ID)
	}
	if fr.shouldCalls != 1 || fr.refreshCalls != 1 {
		t.Errorf("expected 1 ShouldRefresh + 1 Refresh, got %d / %d", fr.shouldCalls, fr.refreshCalls)
	}
	if fr.lastRefreshed != "alice" {
		t.Errorf("refreshed wrong cred: %q", fr.lastRefreshed)
	}
}

func TestConductor_SkipsRefreshWhenFresh(t *testing.T) {
	s := NewDefaultScheduler()
	s.Register([]*Credential{{ID: "alice", Priority: 100}})
	cond := NewConductor(s, &RoundRobinSelector{}, nil)
	fr := &fakeRefresher{shouldRefresh: false}
	cond.SetRefresher(fr)

	if _, err := cond.Acquire(context.Background(), "claude-sonnet-4.6", ""); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if fr.shouldCalls != 1 {
		t.Errorf("ShouldRefresh calls = %d, want 1", fr.shouldCalls)
	}
	if fr.refreshCalls != 0 {
		t.Errorf("Refresh should not be called when ShouldRefresh=false; got %d", fr.refreshCalls)
	}
}

func TestConductor_RefreshFailureDoesNotBlockAcquire(t *testing.T) {
	s := NewDefaultScheduler()
	s.Register([]*Credential{{ID: "alice", Priority: 100}})
	cond := NewConductor(s, &RoundRobinSelector{}, nil)
	cond.SetRefresher(&fakeRefresher{shouldRefresh: true, refreshErr: errBoom})

	got, err := cond.Acquire(context.Background(), "claude-sonnet-4.6", "")
	if err != nil {
		t.Fatalf("expected nil error despite refresh failure, got %v", err)
	}
	if got == nil || got.ID != "alice" {
		t.Fatalf("expected cred alice, got %+v", got)
	}
}

var errBoom = stringError("boom")

type stringError string

func (e stringError) Error() string { return string(e) }

func TestJSONFileRefresher_RefreshErrorLeavesCredUnchanged(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer ts.Close()
	base, _ := url.Parse(ts.URL)
	httpClient := &http.Client{Transport: &rewriteTransport{base: base}}

	cred := &Credential{ID: "alice"}
	cred.Credentials = auth.Credentials{
		AccessToken:  "old",
		RefreshToken: "rt",
		ExpiresAt:    time.Now().Unix() - 10,
		Region:       "us-east-1",
		AuthType:     "social",
	}

	r := NewJSONFileRefresher("", func() []*Credential { return []*Credential{cred} }, httpClient)
	if err := r.Refresh(context.Background(), cred); err == nil {
		t.Fatalf("expected error from 500 response")
	}
	cred.Mu.RLock()
	defer cred.Mu.RUnlock()
	if cred.AccessToken != "old" {
		t.Errorf("token mutated after failed refresh: %q", cred.AccessToken)
	}
}
