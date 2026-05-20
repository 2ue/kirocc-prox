package codex

import (
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/niuma/kirocc-pro/internal/oauth"
	"github.com/niuma/kirocc-pro/internal/provider"
)

// rewriteTransport routes any outbound request whose host matches
// auth.openai.com / chatgpt.com to the local httptest base URL.
type rewriteTransport struct{ base *url.URL }

func (t *rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.Contains(r.URL.Host, "openai.com") || strings.Contains(r.URL.Host, "chatgpt.com") {
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

func TestProvider_HandlesModel(t *testing.T) {
	p := New(nil)
	cases := map[string]bool{
		"gpt-4o":          true,
		"gpt-5.4-mini":    true,
		"o1-preview":      true,
		"o3-mini":         true,
		"codex-something": true,
		"claude-sonnet":   false,
		"gemini-pro":      false,
		"":                false,
	}
	for m, want := range cases {
		if got := p.HandlesModel(m); got != want {
			t.Errorf("HandlesModel(%q) = %v, want %v", m, got, want)
		}
	}
}

func TestStartOAuth_BuildsURL(t *testing.T) {
	// Skip if port 1455 is locally in use (the real Codex flow can't
	// bind elsewhere — OpenAI's auth server enforces the URI verbatim).
	p := New(nil)
	flow, err := p.StartOAuth(context.Background(), nil)
	if err != nil {
		t.Skipf("StartOAuth (port 1455 likely busy): %v", err)
	}
	defer flow.Loopback.Close()

	if !strings.HasPrefix(flow.AuthURL, "https://auth.openai.com/oauth/authorize?") {
		t.Errorf("auth URL has wrong base: %q", flow.AuthURL)
	}
	for _, frag := range []string{
		"client_id=" + clientID,
		"code_challenge_method=S256",
		"codex_cli_simplified_flow=true",
		"id_token_add_organizations=true",
		"prompt=login",
		"response_type=code",
		"redirect_uri=http%3A%2F%2Flocalhost%3A1455%2Fauth%2Fcallback",
	} {
		if !strings.Contains(flow.AuthURL, frag) {
			t.Errorf("auth URL missing %q", frag)
		}
	}
	if flow.State == "" {
		t.Errorf("expected non-empty state")
	}
	if flow.Loopback.Port() != 1455 {
		t.Errorf("expected port 1455, got %d", flow.Loopback.Port())
	}
}

// buildIDToken assembles a JWT-shaped string carrying the OpenAI auth
// claim with the given account_id and plan_type.
func buildIDToken(t *testing.T, accountID, plan string) string {
	t.Helper()
	claims := map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": accountID,
			"chatgpt_plan_type":  plan,
		},
	}
	body, _ := json.Marshal(claims)
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString(body)
	return header + "." + payload + ".unsigned"
}

func TestCompleteOAuth_ExtractsAccountIDFromIDToken(t *testing.T) {
	idTok := buildIDToken(t, "acc_12345", "pro")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		if r.PostFormValue("grant_type") != "authorization_code" {
			t.Errorf("grant_type = %q", r.PostFormValue("grant_type"))
		}
		resp := map[string]any{
			"access_token":  "at-abc",
			"refresh_token": "rt-abc",
			"id_token":      idTok,
			"expires_in":    3600,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.MarshalWrite(w, resp)
	}))
	defer ts.Close()
	base, _ := url.Parse(ts.URL)
	p := New(&http.Client{Transport: &rewriteTransport{base: base}})

	// Build a Flow with an ephemeral loopback (port 0).
	lb, err := oauth.NewLoopback(context.Background(), []int{0}, "/auth/callback")
	if err != nil {
		t.Fatalf("loopback: %v", err)
	}
	defer lb.Close()

	flow := &provider.OAuthFlow{
		AuthURL:  "https://auth.openai.com/oauth/authorize?…",
		State:    "s1",
		Verifier: strings.Repeat("v", 128),
		Loopback: lb,
	}
	params := url.Values{
		"code":  []string{"test-code"},
		"state": []string{"s1"},
	}
	cred, err := p.CompleteOAuth(context.Background(), params, flow, nil)
	if err != nil {
		t.Fatalf("CompleteOAuth: %v", err)
	}
	if cred.AccessToken != "at-abc" || cred.RefreshToken != "rt-abc" {
		t.Errorf("tokens mismatch: %+v", cred.Credentials)
	}
	if cred.Provider != ID {
		t.Errorf("provider = %q, want %q", cred.Provider, ID)
	}
	if got := cred.Metadata["chatgpt_account_id"]; got != "acc_12345" {
		t.Errorf("chatgpt_account_id = %q", got)
	}
	if got := cred.Metadata["chatgpt_plan_type"]; got != "pro" {
		t.Errorf("chatgpt_plan_type = %q", got)
	}
	if !strings.Contains(cred.Label, "PRO") {
		t.Errorf("label should reflect plan, got %q", cred.Label)
	}
	if cred.ExpiresAt < time.Now().Unix()+3500 {
		t.Errorf("expires_at not set correctly: %d", cred.ExpiresAt)
	}
}

func TestCompleteOAuth_StateMismatch(t *testing.T) {
	p := New(nil)
	lb, err := oauth.NewLoopback(context.Background(), []int{0}, "/auth/callback")
	if err != nil {
		t.Fatalf("loopback: %v", err)
	}
	defer lb.Close()
	flow := &provider.OAuthFlow{State: "expected", Verifier: "v", Loopback: lb}
	params := url.Values{"code": []string{"c"}, "state": []string{"wrong"}}
	if _, err := p.CompleteOAuth(context.Background(), params, flow, nil); err == nil {
		t.Fatal("expected state mismatch error")
	}
}

func TestRefreshToken_RotatesAccessToken(t *testing.T) {
	idTok := buildIDToken(t, "acc_old", "plus")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		if r.PostFormValue("grant_type") != "refresh_token" {
			t.Errorf("grant_type = %q", r.PostFormValue("grant_type"))
		}
		resp := map[string]any{
			"access_token":  "at-new",
			"refresh_token": "rt-new",
			"id_token":      idTok,
			"expires_in":    7200,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.MarshalWrite(w, resp)
	}))
	defer ts.Close()
	base, _ := url.Parse(ts.URL)
	p := New(&http.Client{Transport: &rewriteTransport{base: base}})

	cred := makeCred("at-old", "rt-old", time.Now().Unix()-100)
	if err := p.RefreshToken(context.Background(), cred); err != nil {
		t.Fatalf("RefreshToken: %v", err)
	}
	if cred.AccessToken != "at-new" {
		t.Errorf("access token not rotated: %q", cred.AccessToken)
	}
	if cred.RefreshToken != "rt-new" {
		t.Errorf("refresh token not rotated: %q", cred.RefreshToken)
	}
	if cred.Metadata["chatgpt_account_id"] != "acc_old" {
		t.Errorf("account id not re-extracted from new id_token: %q", cred.Metadata["chatgpt_account_id"])
	}
}

func TestFetchQuota_NoEndpointReturnsEmptySnapshot(t *testing.T) {
	p := New(nil)
	cred := makeCred("at", "rt", 0)
	cred.Metadata = map[string]string{"chatgpt_plan_type": "plus"}
	snap, err := p.FetchQuota(context.Background(), cred)
	if err != nil {
		t.Fatalf("FetchQuota: %v", err)
	}
	if snap == nil {
		t.Fatal("expected non-nil snapshot")
	}
	if snap.PlanTier != "plus" {
		t.Errorf("plan tier = %q", snap.PlanTier)
	}
	if !strings.Contains(snap.PlanName, "Plus") {
		t.Errorf("plan name should be human-readable: %q", snap.PlanName)
	}
	if snap.CreditsTotal != 0 || snap.CreditsUsed != 0 {
		t.Errorf("credits should be zero (no endpoint): %+v", snap)
	}
}
