package quota

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const sampleUsageResponse = `{
  "subscriptionInfo": {
    "subscriptionTitle": "Kiro Pro",
    "type": "PRO"
  },
  "usageBreakdownList": [
    {
      "usageLimitWithPrecision": 1000.5,
      "currentUsageWithPrecision": 142.25,
      "freeTrialInfo": {
        "usageLimitWithPrecision": 50.0,
        "currentUsageWithPrecision": 12.5,
        "daysRemaining": 7
      }
    }
  ],
  "nextDateReset": 1735689600
}`

func TestKiroFetcher_Success(t *testing.T) {
	const wantARN = "arn:aws:codewhisperer:us-east-1:123456789012:profile/ABCD?with&special chars"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if got, want := r.URL.Path, "/getUsageLimits"; got != want {
			t.Errorf("path = %s, want %s", got, want)
		}
		if got := r.URL.Query().Get("origin"); got != "AI_EDITOR" {
			t.Errorf("origin = %s, want AI_EDITOR", got)
		}
		if got := r.URL.Query().Get("resourceType"); got != "AGENTIC_REQUEST" {
			t.Errorf("resourceType = %s, want AGENTIC_REQUEST", got)
		}
		// profileArn must arrive url-decoded equal to wantARN.
		if got := r.URL.Query().Get("profileArn"); got != wantARN {
			t.Errorf("profileArn = %q, want %q", got, wantARN)
		}
		// Verify the RAW query contains the url-encoded form.
		if !strings.Contains(r.URL.RawQuery, "profileArn="+url.QueryEscape(wantARN)) {
			t.Errorf("raw query missing escaped profileArn: %s", r.URL.RawQuery)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("authorization = %s, want Bearer test-token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(sampleUsageResponse))
	}))
	defer srv.Close()

	f := NewKiroFetcher(nil)
	// Point endpoint at the test server via a wrapping transport that
	// rewrites the upstream URL.
	f.httpClient = &http.Client{
		Timeout: 5 * time.Second,
		Transport: &rewriteTransport{base: srv.URL, rt: http.DefaultTransport},
	}

	snap, err := f.Fetch(context.Background(), "test-token", wantARN, "us-east-1")
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if snap == nil {
		t.Fatal("nil snapshot")
	}
	if snap.PlanName != "Kiro Pro" {
		t.Errorf("PlanName = %q, want Kiro Pro", snap.PlanName)
	}
	if snap.PlanTier != "PRO" {
		t.Errorf("PlanTier = %q, want PRO", snap.PlanTier)
	}
	if snap.CreditsTotal != 1000.5 {
		t.Errorf("CreditsTotal = %v, want 1000.5", snap.CreditsTotal)
	}
	if snap.CreditsUsed != 142.25 {
		t.Errorf("CreditsUsed = %v, want 142.25", snap.CreditsUsed)
	}
	if snap.BonusTotal != 50.0 {
		t.Errorf("BonusTotal = %v, want 50", snap.BonusTotal)
	}
	if snap.BonusUsed != 12.5 {
		t.Errorf("BonusUsed = %v, want 12.5", snap.BonusUsed)
	}
	if snap.BonusExpireDays != 7 {
		t.Errorf("BonusExpireDays = %d, want 7", snap.BonusExpireDays)
	}
	if snap.NextResetAt.Unix() != 1735689600 {
		t.Errorf("NextResetAt = %d, want 1735689600", snap.NextResetAt.Unix())
	}
	if snap.FetchedAt.IsZero() {
		t.Error("FetchedAt is zero")
	}
	if snap.Banned {
		t.Error("Banned = true, want false")
	}
}

func TestKiroFetcher_PlanNameAliases(t *testing.T) {
	body := `{"planName": "Aliased Plan", "planTier": "FREE", "usageBreakdownList": []}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	f := NewKiroFetcher(&http.Client{
		Transport: &rewriteTransport{base: srv.URL, rt: http.DefaultTransport},
	})
	snap, err := f.Fetch(context.Background(), "tok", "arn", "us-east-1")
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if snap.PlanName != "Aliased Plan" {
		t.Errorf("PlanName = %q, want Aliased Plan", snap.PlanName)
	}
	if snap.PlanTier != "FREE" {
		t.Errorf("PlanTier = %q, want FREE", snap.PlanTier)
	}
}

func TestKiroFetcher_Banned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message": "BANNED: account misuse detected"}`))
	}))
	defer srv.Close()

	f := NewKiroFetcher(&http.Client{
		Transport: &rewriteTransport{base: srv.URL, rt: http.DefaultTransport},
	})
	snap, err := f.Fetch(context.Background(), "tok", "arn", "us-east-1")
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if !snap.Banned {
		t.Error("Banned = false, want true")
	}
	if snap.BanReason != "account misuse detected" {
		t.Errorf("BanReason = %q, want %q", snap.BanReason, "account misuse detected")
	}
}

func TestKiroFetcher_BannedCaseInsensitive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`banned: tos violation`))
	}))
	defer srv.Close()

	f := NewKiroFetcher(&http.Client{
		Transport: &rewriteTransport{base: srv.URL, rt: http.DefaultTransport},
	})
	snap, err := f.Fetch(context.Background(), "tok", "arn", "us-east-1")
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if !snap.Banned || snap.BanReason != "tos violation" {
		t.Errorf("got Banned=%v reason=%q, want true, \"tos violation\"", snap.Banned, snap.BanReason)
	}
}

func TestKiroFetcher_403NoBanString(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message": "AccessDenied"}`))
	}))
	defer srv.Close()

	f := NewKiroFetcher(&http.Client{
		Transport: &rewriteTransport{base: srv.URL, rt: http.DefaultTransport},
	})
	_, err := f.Fetch(context.Background(), "tok", "arn", "us-east-1")
	if err == nil {
		t.Fatal("expected error for 403 without BANNED, got nil")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error = %v, want it to mention 403", err)
	}
}

func TestKiroFetcher_500Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`oops`))
	}))
	defer srv.Close()

	f := NewKiroFetcher(&http.Client{
		Transport: &rewriteTransport{base: srv.URL, rt: http.DefaultTransport},
	})
	_, err := f.Fetch(context.Background(), "tok", "arn", "us-east-1")
	if err == nil {
		t.Fatal("expected error for 500, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error = %v, want it to mention 500", err)
	}
}

// rewriteTransport rewrites the host of outgoing requests to base.
// This lets us point KiroFetcher at a test server while exercising the real
// URL construction in KiroEndpoint().
type rewriteTransport struct {
	base string // e.g. http://127.0.0.1:1234
	rt   http.RoundTripper
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u, err := url.Parse(t.base)
	if err != nil {
		return nil, err
	}
	req.URL.Scheme = u.Scheme
	req.URL.Host = u.Host
	req.Host = u.Host
	return t.rt.RoundTrip(req)
}
