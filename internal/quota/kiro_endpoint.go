package quota

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/niuma/kirocc-pro/internal/pool"
)

// KiroFetcher implements Fetcher by calling the Kiro getUsageLimits endpoint.
type KiroFetcher struct {
	httpClient *http.Client
}

// NewKiroFetcher returns a KiroFetcher using the given HTTP client. A nil
// client falls back to a default with a 30s timeout.
func NewKiroFetcher(client *http.Client) *KiroFetcher {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &KiroFetcher{httpClient: client}
}

// kiroUsageResponse mirrors the subset of getUsageLimits we care about. Alias
// fields are present so older / newer response shapes are both parsed; we
// pick the first non-empty value in priority order at use time.
type kiroUsageResponse struct {
	SubscriptionInfo *struct {
		SubscriptionTitle string `json:"subscriptionTitle"`
		Type              string `json:"type"`
	} `json:"subscriptionInfo,omitempty"`

	PlanName        string `json:"planName,omitempty"`
	CurrentPlanName string `json:"currentPlanName,omitempty"`
	PlanTier        string `json:"planTier,omitempty"`

	// Kiro returns this as a Unix-seconds timestamp but the server has
	// been observed to emit it in scientific notation (e.g. "1.780272E9").
	// json/v2 rejects that when binding to int64 — decode as float64 and
	// truncate below.
	NextDateReset float64 `json:"nextDateReset,omitempty"`

	UsageBreakdownList []struct {
		UsageLimitWithPrecision   float64 `json:"usageLimitWithPrecision"`
		CurrentUsageWithPrecision float64 `json:"currentUsageWithPrecision"`
		FreeTrialInfo             *struct {
			UsageLimitWithPrecision   float64 `json:"usageLimitWithPrecision"`
			CurrentUsageWithPrecision float64 `json:"currentUsageWithPrecision"`
			DaysRemaining             int     `json:"daysRemaining"`
		} `json:"freeTrialInfo,omitempty"`
	} `json:"usageBreakdownList,omitempty"`
}

// Fetch calls GET getUsageLimits and returns the parsed snapshot. A 403 with
// a body containing "BANNED:" is returned as a snapshot with Banned=true
// (NOT an error) — the caller treats banned credentials specially.
func (f *KiroFetcher) Fetch(ctx context.Context, accessToken, profileARN, region string) (*pool.KiroQuotaSnapshot, error) {
	endpoint := KiroEndpoint(region) +
		"/getUsageLimits" +
		"?origin=AI_EDITOR" +
		"&profileArn=" + url.QueryEscape(profileARN) +
		"&resourceType=AGENTIC_REQUEST"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := f.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return nil, fmt.Errorf("read body: %w", readErr)
	}

	if resp.StatusCode == http.StatusForbidden {
		// 403 may carry a BANNED:<reason> string. Capture it as a snapshot so
		// the caller can MarkAuthError without losing the reason.
		reason, banned := parseBanReason(body)
		if banned {
			return &pool.KiroQuotaSnapshot{
				FetchedAt: time.Now(),
				Banned:    true,
				BanReason: reason,
			}, nil
		}
		return nil, fmt.Errorf("getUsageLimits: status 403: %s", truncate(string(body), 256))
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("getUsageLimits: status %d: %s", resp.StatusCode, truncate(string(body), 256))
	}

	var raw kiroUsageResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	snap := &pool.KiroQuotaSnapshot{FetchedAt: time.Now()}

	// PlanName: subscriptionInfo.subscriptionTitle, then planName, then currentPlanName.
	if raw.SubscriptionInfo != nil && raw.SubscriptionInfo.SubscriptionTitle != "" {
		snap.PlanName = raw.SubscriptionInfo.SubscriptionTitle
	} else if raw.PlanName != "" {
		snap.PlanName = raw.PlanName
	} else {
		snap.PlanName = raw.CurrentPlanName
	}

	// PlanTier: subscriptionInfo.type, then planTier.
	if raw.SubscriptionInfo != nil && raw.SubscriptionInfo.Type != "" {
		snap.PlanTier = raw.SubscriptionInfo.Type
	} else {
		snap.PlanTier = raw.PlanTier
	}

	if len(raw.UsageBreakdownList) > 0 {
		b := raw.UsageBreakdownList[0]
		snap.CreditsTotal = b.UsageLimitWithPrecision
		snap.CreditsUsed = b.CurrentUsageWithPrecision
		if b.FreeTrialInfo != nil {
			snap.BonusTotal = b.FreeTrialInfo.UsageLimitWithPrecision
			snap.BonusUsed = b.FreeTrialInfo.CurrentUsageWithPrecision
			snap.BonusExpireDays = b.FreeTrialInfo.DaysRemaining
		}
	}

	if raw.NextDateReset > 0 {
		snap.NextResetAt = time.Unix(int64(raw.NextDateReset), 0).UTC()
	}

	return snap, nil
}

// parseBanReason returns (reason, true) when the body contains "BANNED:"
// (case-insensitive). The reason is everything after the first match up to
// the next quote, brace, or newline. Returns ("", false) otherwise.
func parseBanReason(body []byte) (string, bool) {
	s := string(body)
	lower := strings.ToLower(s)
	idx := strings.Index(lower, "banned:")
	if idx < 0 {
		return "", false
	}
	rest := s[idx+len("banned:"):]
	// Trim leading whitespace.
	rest = strings.TrimLeft(rest, " \t")
	// Stop at the first delimiter likely to terminate the reason.
	end := strings.IndexAny(rest, "\"'\n\r}")
	if end >= 0 {
		rest = rest[:end]
	}
	return strings.TrimSpace(rest), true
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
