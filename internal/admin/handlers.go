package admin

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/niuma/kirocc-pro/internal/pool"
	"github.com/niuma/kirocc-pro/internal/usage"
)

// --- JSON response shapes ---------------------------------------------------

// healthResponse is returned by GET /admin/health.
type healthResponse struct {
	TotalAccounts         int       `json:"total_accounts"`
	Active                int       `json:"active"`
	Cooldown              int       `json:"cooldown"`
	Disabled              int       `json:"disabled"`
	TotalCreditsRemaining float64   `json:"total_credits_remaining"`
	GeneratedAt           time.Time `json:"generated_at"`
	// AdminKeySet is false when the server is running in open mode (no
	// -admin-key). The dashboard surfaces this as a top-bar warning so the
	// operator can see at a glance that no authentication is enforced.
	AdminKeySet bool `json:"admin_key_set"`
	// MultiAccount is true when a durable account store is attached.
	MultiAccount bool `json:"multi_account"`
}

// creditsBlock is the credits section of an account view.
type creditsBlock struct {
	Total     float64 `json:"total"`
	Used      float64 `json:"used"`
	Remaining float64 `json:"remaining"`
}

// bonusBlock is the bonus / free-trial section of an account view.
type bonusBlock struct {
	Total         float64 `json:"total"`
	Used          float64 `json:"used"`
	ExpiresInDays int     `json:"expires_in_days"`
}

// stats24h captures 24h request/token totals for one credential.
type stats24h struct {
	Requests     int64 `json:"requests"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// accountRow is one entry in the GET /admin/accounts response.
type accountRow struct {
	ID              string           `json:"id"`
	Label           string           `json:"label"`
	Provider        string           `json:"provider"`
	Status          string           `json:"status"`
	PlanName        string           `json:"plan_name"`
	AuthType        string           `json:"auth_type"`
	Region          string           `json:"region"`
	ProfileARN      string           `json:"profile_arn"`
	ProxyURL        string           `json:"proxy_url,omitempty"`
	MaxInFlight     int              `json:"max_in_flight,omitempty"`
	InFlight        int64            `json:"in_flight"`
	InFlightByModel map[string]int64 `json:"in_flight_by_model,omitempty"`
	Credits         creditsBlock     `json:"credits"`
	Bonus           bonusBlock       `json:"bonus"`
	NextResetAt     time.Time        `json:"next_reset_at"`
	Stats24h        stats24h         `json:"stats_24h"`
	LastUsedAt      time.Time        `json:"last_used_at"`
	LastQuotaAt     time.Time        `json:"last_quota_at"`
	CooldownUntil   time.Time        `json:"cooldown_until"`
}

// accountDetail extends accountRow with full unmasked detail.
type accountDetail struct {
	accountRow
	Email            string                         `json:"email"`
	ProfileARN       string                         `json:"profile_arn"`
	Region           string                         `json:"region"`
	AuthType         string                         `json:"auth_type"`
	Priority         int                            `json:"priority"`
	DisableCooling   bool                           `json:"disable_cooling"`
	DisabledReason   string                         `json:"disabled_reason"`
	DisabledAt       time.Time                      `json:"disabled_at"`
	BackoffLevel     int                            `json:"backoff_level"`
	Success          int64                          `json:"success_total"`
	Failed           int64                          `json:"failed_total"`
	LastQuotaAt      time.Time                      `json:"last_quota_at"`
	LastQuotaError   string                         `json:"last_quota_error"`
	LastQuotaErrorAt time.Time                      `json:"last_quota_error_at"`
	ModelStates      map[string]pool.ModelStateView `json:"model_states,omitempty"`
}

// usageRow is one row of GET /admin/usage. The dimension columns
// (Model / APIKeyID / Label / Device) are populated depending on the
// group= query parameter — only the relevant ones are set.
type usageRow struct {
	Model        string  `json:"model,omitempty"`
	APIKeyID     string  `json:"api_key_id,omitempty"`
	Label        string  `json:"label,omitempty"` // resolved label for api_key group
	Device       string  `json:"device,omitempty"`
	Requests     int64   `json:"requests"`
	Success      int64   `json:"success"`
	Failed       int64   `json:"failed"`
	AvgLatencyMs float64 `json:"avg_latency_ms,omitempty"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
}

// usageResponse is the envelope around the rolled-up rows.
type usageResponse struct {
	WindowStart time.Time   `json:"window_start"`
	WindowEnd   time.Time   `json:"window_end"`
	Group       string      `json:"group"`
	Rows        []usageRow  `json:"rows"`
	Totals      usageTotals `json:"totals"`
}

type usageTotals struct {
	Requests     int64 `json:"requests"`
	Success      int64 `json:"success"`
	Failed       int64 `json:"failed"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// timelineBucketDTO is the JSON shape for one timeline bucket.
type timelineBucketDTO struct {
	Start        time.Time `json:"start"`
	Requests     int64     `json:"requests"`
	Success      int64     `json:"success"`
	Failed       int64     `json:"failed"`
	InputTokens  int64     `json:"input_tokens"`
	OutputTokens int64     `json:"output_tokens"`
}

// timelineResponse wraps Aggregate.Timeline with window meta.
type timelineResponse struct {
	WindowStart time.Time           `json:"window_start"`
	WindowEnd   time.Time           `json:"window_end"`
	Bucket      string              `json:"bucket"`
	Timeline    []timelineBucketDTO `json:"timeline"`
}

// --- helpers ---------------------------------------------------------------

// writeJSON marshals body via encoding/json/v2 and writes it with the given
// status. On marshal failure a 500 is emitted instead.
func writeJSON(w http.ResponseWriter, status int, body any) {
	buf, err := json.Marshal(body)
	if err != nil {
		slog.Error("admin: marshal response failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf)
	_, _ = w.Write([]byte("\n"))
}

// maskEmail returns a masked form of a credential label. Two paths:
//
//  1. Email-shaped: "alice@example.com (Pro)" → "a***@example.com (Pro)"
//  2. Plain label: only the first identifier-looking run is masked;
//     trailing decoration after whitespace or '(' / '[' is preserved.
//     Examples:
//     "kiro-alice-001" → "k*************"     (whole thing masked)
//     "Alice Pro"      → "A**** Pro"
//     "alice"          → "a****"
//
// Labels under 2 chars are returned unchanged (nothing meaningful to mask).
func maskEmail(s string) string {
	if at := strings.IndexByte(s, '@'); at > 0 {
		return s[:1] + "***" + s[at:]
	} else if at == 0 {
		// Malformed "@example.com" (no local part); leave alone.
		return s
	}
	// Boundary characters that mark the end of the masked identifier.
	// Hyphens and underscores are kept inside the identifier so cred IDs
	// like "kiro-alice-001" are masked as a single unit.
	cut := len(s)
	for i, r := range s {
		if r == ' ' || r == '\t' || r == '(' || r == '[' {
			cut = i
			break
		}
	}
	if cut < 2 {
		return s
	}
	return s[:1] + strings.Repeat("*", cut-1) + s[cut:]
}

// parseDuration parses a duration string with a fallback default.
func parseDuration(s string, fallback time.Duration) time.Duration {
	if s == "" {
		return fallback
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}

// statusFor returns one of "active", "cooldown", "disabled", "banned" for
// the given credential view.
func statusFor(v pool.View) string {
	if v.LastQuota != nil && v.LastQuota.Banned {
		return "banned"
	}
	if v.Disabled {
		return "disabled"
	}
	if v.Quota.Exceeded && time.Now().Before(v.Quota.NextRecoverAt) {
		return "cooldown"
	}
	return "active"
}

// buildAccountRow maps a credential view + 24h stats to an accountRow.
// label can be the raw label or pre-masked.
func buildAccountRow(v pool.View, label string, st stats24h) accountRow {
	prov := v.Provider
	if prov == "" {
		prov = "kiro"
	}
	row := accountRow{
		ID:              v.ID,
		Label:           label,
		Provider:        prov,
		Status:          statusFor(v),
		AuthType:        v.AuthType,
		Region:          v.Region,
		ProfileARN:      v.ProfileARN,
		ProxyURL:        v.ProxyURL,
		MaxInFlight:     v.MaxInFlight,
		InFlight:        v.InFlight,
		InFlightByModel: v.InFlightByModel,
		LastUsedAt:      v.LastUsedAt,
		LastQuotaAt:     v.LastQuotaAt,
		Stats24h:        st,
	}
	if v.LastQuota != nil {
		q := v.LastQuota
		row.PlanName = q.PlanName
		row.Credits = creditsBlock{
			Total:     q.CreditsTotal,
			Used:      q.CreditsUsed,
			Remaining: q.CreditsTotal - q.CreditsUsed,
		}
		row.Bonus = bonusBlock{
			Total:         q.BonusTotal,
			Used:          q.BonusUsed,
			ExpiresInDays: q.BonusExpireDays,
		}
		row.NextResetAt = q.NextResetAt
	}
	if v.Quota.Exceeded {
		row.CooldownUntil = v.Quota.NextRecoverAt
	}
	return row
}

// stats24hFor queries the aggregator for the credential's last-24h totals.
func (s *Server) stats24hFor(r *http.Request, credID string) stats24h {
	now := time.Now()
	agg, err := s.agg.Query(r.Context(), usage.Filter{CredentialIDs: []string{credID}}, usage.Window{
		Start: now.Add(-24 * time.Hour),
		End:   now,
	})
	if err != nil {
		slog.Warn("admin: 24h stats query failed", "cred", credID, "err", err)
		return stats24h{}
	}
	return stats24h{
		Requests:     agg.TotalRequests,
		InputTokens:  agg.TotalInputTokens,
		OutputTokens: agg.TotalOutputTokens,
	}
}

// --- API handlers ----------------------------------------------------------

// providerDTO is the JSON shape for one row of /admin/providers.
type providerDTO struct {
	ID           string `json:"id"`
	DisplayName  string `json:"display_name"`
	AccountCount int    `json:"account_count"`
}

func (s *Server) handleProviders(w http.ResponseWriter, _ *http.Request) {
	out := []providerDTO{}
	if s.registry == nil {
		writeJSON(w, http.StatusOK, out)
		return
	}
	// Count credentials per provider.
	counts := map[string]int{}
	for _, c := range s.sched.All() {
		p := c.Provider
		if p == "" {
			p = "kiro"
		}
		counts[p]++
	}
	for _, p := range s.registry.All() {
		out = append(out, providerDTO{
			ID:           p.ID(),
			DisplayName:  p.DisplayName(),
			AccountCount: counts[p.ID()],
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	all := s.sched.All()
	resp := healthResponse{
		TotalAccounts: len(all),
		GeneratedAt:   time.Now(),
		AdminKeySet:   s.adminKey != "",
		MultiAccount:  s.credStore != nil,
	}
	for _, c := range all {
		v := c.Snapshot()
		switch statusFor(v) {
		case "active":
			resp.Active++
		case "cooldown":
			resp.Cooldown++
		case "disabled", "banned":
			resp.Disabled++
		}
		if v.LastQuota != nil {
			rem := v.LastQuota.CreditsTotal - v.LastQuota.CreditsUsed
			if rem > 0 {
				resp.TotalCreditsRemaining += rem
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleAccountsList(w http.ResponseWriter, r *http.Request) {
	all := s.sched.All()
	rows := make([]accountRow, 0, len(all))
	for _, c := range all {
		v := c.Snapshot()
		rows = append(rows, buildAccountRow(v, maskEmail(v.Label), s.stats24hFor(r, v.ID)))
	}
	writeJSON(w, http.StatusOK, rows)
}

func (s *Server) handleAccountGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c := s.sched.Lookup(id)
	if c == nil {
		http.Error(w, "account not found", http.StatusNotFound)
		return
	}
	v := c.Snapshot()
	row := buildAccountRow(v, v.Label, s.stats24hFor(r, v.ID))
	detail := accountDetail{
		accountRow:       row,
		Email:            v.Label,
		ProfileARN:       v.ProfileARN,
		Region:           v.Region,
		AuthType:         v.AuthType,
		Priority:         v.Priority,
		DisableCooling:   v.DisableCooling,
		DisabledReason:   v.DisabledReason,
		DisabledAt:       v.DisabledAt,
		BackoffLevel:     v.Quota.BackoffLevel,
		Success:          v.Success,
		Failed:           v.Failed,
		LastQuotaAt:      v.LastQuotaAt,
		LastQuotaError:   v.LastQuotaError,
		LastQuotaErrorAt: v.LastQuotaErrorAt,
		ModelStates:      v.ModelStates,
	}
	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) handleAccountRefresh(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c := s.sched.Lookup(id)
	if c == nil {
		http.Error(w, "account not found", http.StatusNotFound)
		return
	}

	snap, err := s.fetchQuotaWithRefresh(r.Context(), c, id)
	if err != nil {
		slog.Warn("admin: force refresh failed", "cred", id, "err", err)
		s.sched.RecordQuotaError(id, err.Error())
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	s.sched.RefreshQuota(id, snap)
	writeJSON(w, http.StatusOK, snap)
}

// fetchQuotaWithRefresh queries getUsageLimits using the credential's current
// access token, routed through cred.ProxyURL when set. If that fails with
// an auth-shaped error AND a refresher is attached, it rotates the OAuth
// tokens via refresh_token and retries once.
//
// Routes via the registered kiro Provider (which honors per-account
// proxy URLs) when available; falls back to the cache otherwise.
func (s *Server) fetchQuotaWithRefresh(ctx context.Context, c *pool.Credential, id string) (*pool.KiroQuotaSnapshot, error) {
	fetch := func() (*pool.KiroQuotaSnapshot, error) {
		if s.registry != nil {
			if prov, err := s.registry.Get("kiro"); err == nil {
				return prov.FetchQuota(ctx, c)
			}
		}
		access, arn, region := readCredQuotaInputs(c)
		return s.cache.FetchForce(ctx, id, access, arn, region)
	}
	snap, err := fetch()
	if err == nil {
		return snap, nil
	}
	if s.refresher == nil || !isAuthError(err) {
		return nil, err
	}
	if refreshErr := s.refresher.Refresh(ctx, c); refreshErr != nil {
		return nil, fmt.Errorf("token refresh: %w (original: %v)", refreshErr, err)
	}
	return fetch()
}

func readCredQuotaInputs(c *pool.Credential) (access, arn, region string) {
	c.Mu.RLock()
	defer c.Mu.RUnlock()
	return c.AccessToken, c.ProfileARN, c.Region
}

// isAuthError matches the upstream "invalid bearer token" / "expired"
// shapes that signal we should refresh and retry.
func isAuthError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "status 401") || strings.Contains(s, "status 403") {
		return true
	}
	return strings.Contains(s, "bearer token") ||
		strings.Contains(s, "expired") ||
		strings.Contains(s, "unauthorized")
}

func (s *Server) handleAccountDisable(w http.ResponseWriter, r *http.Request) {
	s.setEnabled(w, r, false)
}

func (s *Server) handleAccountEnable(w http.ResponseWriter, r *http.Request) {
	s.setEnabled(w, r, true)
}

func (s *Server) setEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	id := r.PathValue("id")
	if err := s.sched.SetEnabled(id, enabled); err != nil {
		if errors.Is(err, pool.ErrCredentialNotFound) {
			http.Error(w, "account not found", http.StatusNotFound)
			return
		}
		slog.Error("admin: SetEnabled failed", "cred", id, "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "enabled": enabled})
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	window := parseDuration(q.Get("window"), 24*time.Hour)
	group := q.Get("group")
	if group == "" {
		group = "model"
	}

	now := time.Now()
	win := usage.Window{Start: now.Add(-window), End: now}
	agg, err := s.agg.Query(r.Context(), usage.Filter{}, win)
	if err != nil {
		slog.Error("admin: usage query failed", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := usageResponse{
		WindowStart: win.Start,
		WindowEnd:   win.End,
		Group:       group,
		Rows:        []usageRow{},
		Totals: usageTotals{
			Requests:     agg.TotalRequests,
			Success:      agg.TotalSuccess,
			Failed:       agg.TotalFailed,
			InputTokens:  agg.TotalInputTokens,
			OutputTokens: agg.TotalOutputTokens,
		},
	}

	switch group {
	case "model":
		merged := map[string]*usageRow{}
		// Track per-row pricing model so cost can be computed once at
		// the end with the fully-aggregated CellStats.
		merged2 := map[string]usage.CellStats{}
		for _, byModel := range agg.ByCredModel {
			for model, cell := range byModel {
				row := merged[model]
				if row == nil {
					row = &usageRow{Model: model}
					merged[model] = row
				}
				addCellToRow(row, cell)
				c := merged2[model]
				addToCellAccum(&c, cell)
				merged2[model] = c
			}
		}
		for model, row := range merged {
			c := merged2[model]
			row.AvgLatencyMs = usage.AvgLatencyMs(c)
			resp.Rows = append(resp.Rows, *row)
		}
	case "api_key":
		labels := s.apiKeyLabels()
		for id, cell := range agg.ByAPIKey {
			label := labels[id]
			if id == "" {
				label = "（legacy -api-key）"
			}
			row := usageRow{APIKeyID: id, Label: label}
			addCellToRow(&row, cell)
			row.AvgLatencyMs = usage.AvgLatencyMs(cell)
			// Cost can't be inferred without per-row model; leave 0
			// for the api_key dimension (UI shows it as "—" then).
			resp.Rows = append(resp.Rows, row)
		}
	case "device":
		for id, cell := range agg.ByDevice {
			row := usageRow{Device: id, Label: cell.DeviceLabel}
			addCellToRow(&row, cell)
			row.AvgLatencyMs = usage.AvgLatencyMs(cell)
			resp.Rows = append(resp.Rows, row)
		}
	default:
		http.Error(w, "unsupported group: "+group, http.StatusBadRequest)
		return
	}

	writeJSON(w, http.StatusOK, resp)
}

// recentRecordDTO is the per-row JSON shape for the history panel.
type recentRecordDTO struct {
	Timestamp            time.Time `json:"timestamp"`
	CredentialID         string    `json:"credential_id"`
	CredentialLabel      string    `json:"credential_label,omitempty"`
	RequestPath          string    `json:"request_path"`
	PromptCacheProfile   string    `json:"prompt_cache_profile,omitempty"`
	PromptCachePrefix    string    `json:"prompt_cache_prefix,omitempty"`
	Type                 string    `json:"type"`
	RequestedModel       string    `json:"requested_model"`
	ResolvedModel        string    `json:"resolved_model"`
	Status               string    `json:"status"`
	InputTokens          int       `json:"input_tokens"`
	OutputTokens         int       `json:"output_tokens"`
	CacheReadTokens      int       `json:"cache_read_tokens"`
	CacheWriteTokens     int       `json:"cache_write_tokens"`
	RawInputTokens       int       `json:"raw_input_tokens"`
	RawOutputTokens      int       `json:"raw_output_tokens"`
	RawCacheReadTokens   int       `json:"raw_cache_read_tokens"`
	RawCacheWriteTokens  int       `json:"raw_cache_write_tokens"`
	LatencyMs            int       `json:"latency_ms"`
	FirstTokenMs         int       `json:"first_token_ms"`
	ErrorMessage         string    `json:"error_message,omitempty"`
	Device               string    `json:"device"`
	DeviceID             string    `json:"device_id,omitempty"`
	APIKeyID             string    `json:"api_key_id,omitempty"`
	APIKeyLabel          string    `json:"api_key_label,omitempty"`
	TraceID              string    `json:"trace_id"`
	CreditsUsedSnapshot  float64   `json:"credits_used_snapshot"`
	CreditsTotalSnapshot float64   `json:"credits_total_snapshot"`
}

func (s *Server) handleUsageRecent(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 100
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	filter := usage.Filter{
		CredentialIDs: queryList(q, "cred_id"),
		Models:        queryList(q, "model"),
		Statuses:      queryList(q, "status"),
		APIKeyIDs:     queryList(q, "api_key_id"),
		DeviceIDs:     queryList(q, "device_id"),
	}
	records, err := s.agg.Recent(r.Context(), filter, limit)
	if err != nil {
		slog.Error("admin: recent usage query failed", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	labels := s.apiKeyLabels()
	credLabels := s.credentialLabels()
	out := make([]recentRecordDTO, 0, len(records))
	for _, rec := range records {
		out = append(out, recentRecordDTO{
			Timestamp:            rec.Timestamp,
			CredentialID:         rec.CredentialID,
			CredentialLabel:      credLabels[rec.CredentialID],
			RequestPath:          rec.RequestPath,
			PromptCacheProfile:   rec.PromptCacheProfile,
			PromptCachePrefix:    rec.PromptCachePrefix,
			Type:                 rec.Type,
			RequestedModel:       rec.RequestedModel,
			ResolvedModel:        rec.ResolvedModel,
			Status:               rec.Status,
			InputTokens:          rec.InputTokens,
			OutputTokens:         rec.OutputTokens,
			CacheReadTokens:      rec.CacheReadTokens,
			CacheWriteTokens:     rec.CacheWriteTokens,
			RawInputTokens:       rec.RawInputTokens,
			RawOutputTokens:      rec.RawOutputTokens,
			RawCacheReadTokens:   rec.RawCacheReadTokens,
			RawCacheWriteTokens:  rec.RawCacheWriteTokens,
			LatencyMs:            rec.LatencyMs,
			FirstTokenMs:         rec.FirstTokenMs,
			ErrorMessage:         rec.ErrorMessage,
			Device:               rec.Device,
			DeviceID:             rec.DeviceID,
			APIKeyID:             rec.APIKeyID,
			APIKeyLabel:          labels[rec.APIKeyID],
			TraceID:              rec.TraceID,
			CreditsUsedSnapshot:  rec.CreditsUsedSnapshot,
			CreditsTotalSnapshot: rec.CreditsTotalSnapshot,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// queryList returns the comma-and-repeat-aware list of values for a query
// parameter. ?model=a&model=b and ?model=a,b are both treated as ["a","b"].
func queryList(q map[string][]string, key string) []string {
	raw := q[key]
	if len(raw) == 0 {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		for _, p := range strings.Split(v, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

func (s *Server) handleUsageTimeline(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	window := parseDuration(q.Get("window"), 2*time.Hour)
	bucket := parseDuration(q.Get("bucket"), 10*time.Minute)

	now := time.Now()
	win := usage.Window{
		Start:  now.Add(-window),
		End:    now,
		Bucket: bucket,
	}
	agg, err := s.agg.Query(r.Context(), usage.Filter{}, win)
	if err != nil {
		slog.Error("admin: timeline query failed", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	timeline := make([]timelineBucketDTO, 0, len(agg.Timeline))
	for _, b := range agg.Timeline {
		timeline = append(timeline, timelineBucketDTO{
			Start:        b.Start,
			Requests:     b.Requests,
			Success:      b.Success,
			Failed:       b.Failed,
			InputTokens:  b.InputTokens,
			OutputTokens: b.OutputTokens,
		})
	}
	writeJSON(w, http.StatusOK, timelineResponse{
		WindowStart: win.Start,
		WindowEnd:   win.End,
		Bucket:      bucket.String(),
		Timeline:    timeline,
	})
}

// --- HTML handlers ---------------------------------------------------------

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	// Anything under /admin/ that didn't match an API or asset route falls
	// here and serves the SPA shell.
	serveAsset(w, r, "index.html")
}

func (s *Server) handleAssets(w http.ResponseWriter, r *http.Request) {
	const prefix = "/admin/assets/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, prefix)
	if name == "" || strings.Contains(name, "..") {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	serveAsset(w, r, name)
}

// addCellToRow folds a usage cell into the per-dimension row.
func addCellToRow(row *usageRow, cell usage.CellStats) {
	row.Requests += cell.Requests
	row.Success += cell.Success
	row.Failed += cell.Failed
	row.InputTokens += cell.InputTokens
	row.OutputTokens += cell.OutputTokens
}

// addToCellAccum merges one CellStats into another. Used by the model
// rollup to also keep a CellStats accumulator alongside the usageRow,
// so per-model latency can be computed with the full counts at the end
// of the aggregation loop.
func addToCellAccum(dst *usage.CellStats, src usage.CellStats) {
	dst.Requests += src.Requests
	dst.Success += src.Success
	dst.Failed += src.Failed
	dst.InputTokens += src.InputTokens
	dst.OutputTokens += src.OutputTokens
	dst.TotalLatencyMs += src.TotalLatencyMs
}

// apiKeyLabels returns a snapshot of api_key_id → label so the UI can show
// human names alongside opaque ids in the usage rollup. Empty when no
// settings store is wired.
func (s *Server) apiKeyLabels() map[string]string {
	if s.settings == nil {
		return nil
	}
	cur := s.settings.Get()
	out := make(map[string]string, len(cur.APIKeys))
	for _, k := range cur.APIKeys {
		out[k.ID] = k.Label
	}
	return out
}

// credentialLabels returns credential_id -> display label for recent request
// history. The id is returned separately and the UI can expose it on hover.
func (s *Server) credentialLabels() map[string]string {
	if s.sched == nil {
		return nil
	}
	all := s.sched.All()
	out := make(map[string]string, len(all))
	for _, c := range all {
		if c == nil {
			continue
		}
		v := c.Snapshot()
		label := strings.TrimSpace(v.Label)
		if label == "" {
			label = v.ID
		}
		out[v.ID] = label
	}
	return out
}
