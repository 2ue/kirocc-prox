// [fork] New file added in fork. Implements the account-mutation endpoints
// (create / import / delete) used by the admin dashboard's Accounts page.
// All mutations require a durable CredentialStore; production wires this to
// PostgreSQL.

package admin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/niuma/kirocc-pro/internal/auth"
	"github.com/niuma/kirocc-pro/internal/pool"
)

const maxImportBodyBytes = 16 * 1024 * 1024

// createAccountReq matches the JSON import pool.CredentialFile shape but is
// duplicated here so the HTTP layer is decoupled from any future renames.
type createAccountReq struct {
	ID               string                `json:"id"`
	Label            string                `json:"label,omitempty"`
	Provider         string                `json:"provider,omitempty"`
	Priority         int                   `json:"priority,omitempty"`
	Disabled         bool                  `json:"disabled,omitempty"`
	DisableCooling   bool                  `json:"disable_cooling,omitempty"`
	MaxInFlight      int                   `json:"max_in_flight,omitempty"`
	ProxyURL         string                `json:"proxy_url,omitempty"`
	KiroAuthTokenRaw pool.KiroAuthTokenRaw `json:"kiro_auth_token_raw"`
}

func (r *createAccountReq) toCredentialFile() pool.CredentialFile {
	return pool.CredentialFile{
		ID:               r.ID,
		Label:            r.Label,
		Provider:         r.Provider,
		Priority:         r.Priority,
		Disabled:         r.Disabled,
		DisableCooling:   r.DisableCooling,
		MaxInFlight:      r.MaxInFlight,
		ProxyURL:         r.ProxyURL,
		KiroAuthTokenRaw: r.KiroAuthTokenRaw,
	}
}

type importReq struct {
	Accounts []createAccountReq `json:"accounts"`
	// Mode is "append" (default) or "replace".
	Mode string `json:"mode,omitempty"`
}

type importResp struct {
	Added   int      `json:"added"`
	Skipped int      `json:"skipped"`
	Removed int      `json:"removed,omitempty"`
	Errors  []string `json:"errors,omitempty"`
}

// handleAccountCreate adds a single credential to the pool and persists the
// updated set to PostgreSQL.
func (s *Server) handleAccountCreate(w http.ResponseWriter, r *http.Request) {
	if !s.allowMutate(w) {
		return
	}
	var body createAccountReq
	if err := json.UnmarshalRead(r.Body, &body); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateCreateReq(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	cred, err := credentialFromFile(body.toCredentialFile())
	if err != nil {
		http.Error(w, "convert: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.sched.Add(cred); err != nil {
		if errIsDuplicate(err) {
			http.Error(w, "account id already exists", http.StatusConflict)
			return
		}
		slog.Error("admin: add credential failed", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.persistOne(cred); err != nil {
		// Roll back the in-memory add to keep PostgreSQL and pool consistent.
		_ = s.sched.Remove(cred.ID)
		slog.Error("admin: persist creds failed", "err", err)
		http.Error(w, "persist: "+err.Error(), http.StatusInternalServerError)
		return
	}
	v := cred.Snapshot()
	writeJSON(w, http.StatusCreated, buildAccountRow(v, v.Label, stats24h{}))
}

// patchAccountReq carries optional updates for handleAccountPatch. Nil
// pointers mean "leave unchanged".
type patchAccountReq struct {
	Label       *string `json:"label,omitempty"`
	Priority    *int    `json:"priority,omitempty"`
	MaxInFlight *int    `json:"max_in_flight,omitempty"`
	ProxyURL    *string `json:"proxy_url,omitempty"`
}

// handleAccountPatch updates the mutable, non-secret fields on an
// existing credential (label, priority, proxy_url) and persists the
// pool to PostgreSQL. To rotate tokens, use POST /admin/accounts/{id}/refresh.
func (s *Server) handleAccountPatch(w http.ResponseWriter, r *http.Request) {
	if !s.allowMutate(w) {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	c := s.sched.Lookup(id)
	if c == nil {
		http.Error(w, "account not found", http.StatusNotFound)
		return
	}
	var body patchAccountReq
	if err := json.UnmarshalRead(r.Body, &body); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.MaxInFlight != nil && *body.MaxInFlight < 0 {
		http.Error(w, "max_in_flight must be >= 0", http.StatusBadRequest)
		return
	}
	before := cloneCredentials(s.sched.All())
	c.Mu.Lock()
	if body.Label != nil {
		c.Label = *body.Label
	}
	if body.Priority != nil {
		c.Priority = *body.Priority
	}
	if body.MaxInFlight != nil {
		c.MaxInFlight = *body.MaxInFlight
	}
	if body.ProxyURL != nil {
		c.ProxyURL = strings.TrimSpace(*body.ProxyURL)
	}
	c.Mu.Unlock()
	if err := s.persistOne(c); err != nil {
		s.sched.Register(before)
		slog.Error("admin: persist after patch failed", "err", err)
		http.Error(w, "persist: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, buildAccountRow(c.Snapshot(), c.Label, s.stats24hFor(r, id)))
}

// handleAccountDelete removes a credential and persists.
func (s *Server) handleAccountDelete(w http.ResponseWriter, r *http.Request) {
	if !s.allowMutate(w) {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	before := cloneCredentials(s.sched.All())
	if err := s.sched.Remove(id); err != nil {
		http.Error(w, "account not found", http.StatusNotFound)
		return
	}
	if err := s.deletePersistedCred(r.Context(), id); err != nil {
		s.sched.Register(before)
		slog.Error("admin: persist after delete failed", "err", err)
		http.Error(w, "persist: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAccountsImport bulk-loads accounts from a JSON payload. The
// payload may be a raw array (pool.CredentialFile shape) or an envelope
// {"accounts": [...], "mode": "append|replace"}.
//
// "append" (default) skips entries whose ID already exists; "replace"
// removes any existing accounts not in the payload before adding.
func (s *Server) handleAccountsImport(w http.ResponseWriter, r *http.Request) {
	if !s.allowMutate(w) {
		return
	}
	body, err := decodeImport(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if body.Mode == "" {
		body.Mode = strings.TrimSpace(r.URL.Query().Get("mode"))
	}
	if len(body.Accounts) == 0 {
		http.Error(w, "accounts array is empty", http.StatusBadRequest)
		return
	}

	resp := importResp{}
	before := cloneCredentials(s.sched.All())
	mode := strings.ToLower(body.Mode)
	if mode == "" {
		mode = "append"
	}
	if mode != "append" && mode != "replace" {
		http.Error(w, "mode must be 'append' or 'replace'", http.StatusBadRequest)
		return
	}

	prepared := make([]*pool.Credential, 0, len(body.Accounts))
	for i := range body.Accounts {
		entry := &body.Accounts[i]
		cred, err := s.prepareImportedCredential(r.Context(), entry)
		if err != nil {
			resp.Errors = append(resp.Errors, fmt.Sprintf("[%d] %s: %v", i, entry.ID, err))
			continue
		}
		prepared = append(prepared, cred)
	}

	// Replace mode should not delete the current pool unless the incoming
	// credentials have all passed final validation.
	if mode == "replace" && len(resp.Errors) > 0 {
		writeJSON(w, http.StatusOK, resp)
		return
	}

	mutated := false
	if mode == "replace" {
		incoming := make(map[string]struct{}, len(prepared))
		for _, c := range prepared {
			if c.ID != "" {
				incoming[c.ID] = struct{}{}
			}
		}
		for _, c := range s.sched.All() {
			if _, keep := incoming[c.ID]; !keep {
				if err := s.sched.Remove(c.ID); err == nil {
					resp.Removed++
					mutated = true
				}
			}
		}
	}

	for _, cred := range prepared {
		if err := s.sched.Add(cred); err != nil {
			if errIsDuplicate(err) {
				resp.Skipped++
				continue
			}
			resp.Errors = append(resp.Errors, fmt.Sprintf("%s: %v", cred.ID, err))
			continue
		}
		resp.Added++
		mutated = true
	}

	if mutated {
		if err := s.persistCreds(); err != nil {
			s.sched.Register(before)
			slog.Error("admin: persist after import failed", "err", err)
			http.Error(w, "persist: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// decodeImport reads either {"accounts": [...]} or a raw array.
func decodeImport(r *http.Request) (importReq, error) {
	data, err := io.ReadAll(io.LimitReader(r.Body, maxImportBodyBytes+1))
	if err != nil {
		return importReq{}, fmt.Errorf("read body: %w", err)
	}
	if len(data) > maxImportBodyBytes {
		return importReq{}, fmt.Errorf("import payload exceeds %d bytes", maxImportBodyBytes)
	}
	return decodeImportBytes(data)
}

func decodeImportBytes(data []byte) (importReq, error) {
	values, err := parseImportValues(data)
	if err != nil {
		return importReq{}, err
	}
	req := importReq{Mode: "append"}
	for _, raw := range values {
		accounts, mode, err := importAccountsFromValue(raw, len(req.Accounts))
		if err != nil {
			return importReq{}, err
		}
		if mode != "" {
			req.Mode = mode
		}
		req.Accounts = append(req.Accounts, accounts...)
	}
	return req, nil
}

func parseImportValues(data []byte) ([]any, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, fmt.Errorf("empty import payload")
	}
	var raw any
	if err := json.Unmarshal(data, &raw); err == nil {
		return []any{raw}, nil
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var values []any
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var lineRaw any
		if err := json.Unmarshal([]byte(line), &lineRaw); err != nil {
			return nil, fmt.Errorf("invalid JSON or JSONL at line %d: %w", lineNo, err)
		}
		values = append(values, lineRaw)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read JSONL: %w", err)
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("empty import payload")
	}
	return values, nil
}

func importAccountsFromValue(raw any, offset int) ([]createAccountReq, string, error) {
	raw = camelizeImportValue(raw)
	items := extractImportCredentialItems(raw)
	if len(items) == 0 {
		return nil, "", fmt.Errorf("expected credential object, array, or wrapper with accounts/credentials")
	}
	accounts := make([]createAccountReq, 0, len(items))
	for i, item := range items {
		acct, err := normalizeImportAccount(item, offset+i)
		if err != nil {
			return nil, "", err
		}
		accounts = append(accounts, acct)
	}
	mode := ""
	if obj, ok := raw.(map[string]any); ok {
		mode = stringValue(obj, "mode")
	}
	return accounts, mode, nil
}

func camelizeImportValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return camelizeMapKeys(x)
	case []any:
		arr := make([]any, len(x))
		for i, item := range x {
			arr[i] = camelizeImportValue(item)
		}
		return arr
	default:
		return v
	}
}

func extractImportCredentialItems(v any) []any {
	switch x := v.(type) {
	case []any:
		var out []any
		for _, item := range x {
			out = append(out, extractImportCredentialItems(item)...)
		}
		return out
	case map[string]any:
		for _, key := range []string{"accounts", "credentials"} {
			if arr, ok := x[key].([]any); ok {
				return extractImportCredentialItems(arr)
			}
		}
		if data, ok := x["data"].(map[string]any); ok {
			if arr, ok := data["credentials"].([]any); ok {
				return extractImportCredentialItems(arr)
			}
		}
		return []any{x}
	default:
		return nil
	}
}

func normalizeImportAccount(v any, idx int) (createAccountReq, error) {
	obj, ok := v.(map[string]any)
	if !ok {
		return createAccountReq{}, fmt.Errorf("[%d] expected credential object", idx)
	}
	obj = camelizeMapKeys(obj)
	nested := nestedObject(obj, "credentials")
	tokenRaw := nestedObject(obj, "kiroAuthTokenRaw")
	if len(tokenRaw) == 0 {
		tokenRaw = obj
	}
	if len(nested) > 0 {
		tokenRaw = mergeMissing(tokenRaw, nested)
	}

	authMethod := firstString(obj, nested, tokenRaw, "authMethod")
	apiKey := firstString(obj, nested, tokenRaw, "kiroApiKey")
	if apiKey == "" {
		apiKey = firstString(obj, nested, tokenRaw, "apiKey")
	}
	if apiKey != "" && authMethod == "" {
		authMethod = "api_key"
	}

	id := firstString(obj, nil, nil, "id")
	if id == "" {
		id = firstString(obj, nil, nil, "credentialId")
	}
	if id == "" {
		id = defaultCredentialID(firstString(obj, nested, tokenRaw, "email"), firstString(obj, nested, tokenRaw, "nickname"), firstString(tokenRaw, nil, nil, "profileArn"), idx)
	}
	label := firstString(obj, nil, nil, "label")
	if label == "" {
		label = firstString(obj, nested, tokenRaw, "email")
	}
	if label == "" {
		label = firstString(obj, nested, tokenRaw, "nickname")
	}

	region := firstString(tokenRaw, nested, obj, "region")
	authRegion := firstString(obj, nested, tokenRaw, "authRegion")
	apiRegion := firstString(obj, nested, tokenRaw, "apiRegion")
	ssoRegion := firstString(tokenRaw, nested, obj, "ssoRegion")
	if authRegion != "" {
		ssoRegion = authRegion
	}
	if apiRegion != "" {
		region = apiRegion
	} else if region == "" && authRegion != "" {
		region = authRegion
	}

	req := createAccountReq{
		ID:             id,
		Label:          label,
		Provider:       firstString(obj, nil, nil, "provider"),
		Priority:       intValue(obj, "priority"),
		Disabled:       boolValue(obj, "disabled"),
		DisableCooling: boolValue(obj, "disableCooling"),
		MaxInFlight:    firstInt(obj, nested, tokenRaw, "maxInFlight", "maxConcurrentRequests"),
		ProxyURL:       firstString(obj, nested, tokenRaw, "proxyUrl"),
		KiroAuthTokenRaw: pool.KiroAuthTokenRaw{
			AccessToken:  firstString(tokenRaw, nested, obj, "accessToken"),
			RefreshToken: firstString(tokenRaw, nested, obj, "refreshToken"),
			ExpiresAt:    firstString(tokenRaw, nested, obj, "expiresAt"),
			ProfileARN:   firstString(tokenRaw, nested, obj, "profileArn"),
			AuthMethod:   authMethod,
			Region:       region,
			SSORegion:    ssoRegion,
			ClientID:     firstString(tokenRaw, nested, obj, "clientId"),
			ClientSecret: firstString(tokenRaw, nested, obj, "clientSecret"),
		},
	}
	if req.Provider == "" {
		req.Provider = "kiro"
	}
	if strings.EqualFold(req.KiroAuthTokenRaw.AuthMethod, "api_key") || apiKey != "" {
		return createAccountReq{}, fmt.Errorf("[%d] API Key 凭据暂不支持导入到当前 Kiro 多账号池，请使用 OAuth/refreshToken 凭据", idx)
	}
	return req, nil
}

func camelizeMapKeys(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		key := camelizeKey(k)
		switch vv := v.(type) {
		case map[string]any:
			out[key] = camelizeMapKeys(vv)
		case []any:
			arr := make([]any, len(vv))
			for i, item := range vv {
				if m, ok := item.(map[string]any); ok {
					arr[i] = camelizeMapKeys(m)
				} else {
					arr[i] = item
				}
			}
			out[key] = arr
		default:
			out[key] = v
		}
	}
	return out
}

func camelizeKey(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == '_' || r == '-' || r == ' '
	})
	if len(parts) <= 1 {
		return normalizeImportKey(strings.ToLower(s[:1]) + s[1:])
	}
	for i := range parts {
		if parts[i] == "" {
			continue
		}
		parts[i] = strings.ToLower(parts[i])
		if i > 0 {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
	}
	return normalizeImportKey(strings.Join(parts, ""))
}

func normalizeImportKey(s string) string {
	compact := strings.ToLower(strings.NewReplacer("_", "", "-", "", " ", "").Replace(s))
	switch compact {
	case "id":
		return "id"
	case "credentialid":
		return "credentialId"
	case "profilearn":
		return "profileArn"
	case "ssoregion":
		return "ssoRegion"
	case "authregion":
		return "authRegion"
	case "apiregion":
		return "apiRegion"
	case "clientid":
		return "clientId"
	case "clientsecret":
		return "clientSecret"
	case "proxyurl":
		return "proxyUrl"
	case "refreshtoken":
		return "refreshToken"
	case "accesstoken":
		return "accessToken"
	case "expiresat":
		return "expiresAt"
	case "authmethod":
		return "authMethod"
	case "kiroapikey":
		return "kiroApiKey"
	case "apikey":
		return "apiKey"
	case "disablecooling":
		return "disableCooling"
	case "maxinflight":
		return "maxInFlight"
	case "maxconcurrentrequests":
		return "maxConcurrentRequests"
	default:
		return s
	}
}

func nestedObject(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	if v, ok := m[key].(map[string]any); ok {
		return v
	}
	return nil
}

func mergeMissing(base, fallback map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(fallback))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range fallback {
		if _, ok := out[k]; !ok {
			out[k] = v
		}
	}
	return out
}

func firstString(a, b, c map[string]any, key string) string {
	for _, m := range []map[string]any{a, b, c} {
		if s := stringValue(m, key); s != "" {
			return s
		}
	}
	return ""
}

func stringValue(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if s, ok := m[key].(string); ok {
		return strings.TrimSpace(s)
	}
	if f, ok := m[key].(float64); ok && f == float64(int64(f)) {
		return strconv.FormatInt(int64(f), 10)
	}
	return ""
}

func intValue(m map[string]any, key string) int {
	n, _ := parseIntValue(m, key)
	return n
}

func parseIntValue(m map[string]any, key string) (int, bool) {
	if m == nil {
		return 0, false
	}
	switch v := m[key].(type) {
	case float64:
		if v == float64(int(v)) {
			return int(v), true
		}
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err == nil {
			return n, true
		}
	}
	return 0, false
}

func boolValue(m map[string]any, key string) bool {
	if m == nil {
		return false
	}
	switch v := m[key].(type) {
	case bool:
		return v
	case string:
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		return err == nil && b
	}
	return false
}

func firstInt(a, b, c map[string]any, keys ...string) int {
	for _, key := range keys {
		for _, m := range []map[string]any{a, b, c} {
			if n, ok := parseIntValue(m, key); ok {
				return n
			}
		}
	}
	return 0
}

func defaultCredentialID(email, nickname, profileARN string, idx int) string {
	base := email
	if base == "" {
		base = nickname
	}
	if base == "" && profileARN != "" {
		if i := strings.LastIndex(profileARN, "/"); i >= 0 && i+1 < len(profileARN) {
			base = profileARN[i+1:]
		} else {
			base = profileARN
		}
	}
	if base == "" {
		base = fmt.Sprintf("imported-%d", idx+1)
	}
	base = strings.ToLower(strings.TrimSpace(base))
	base = regexp.MustCompile(`[^a-z0-9._@-]+`).ReplaceAllString(base, "-")
	base = strings.Trim(base, "-")
	if base == "" {
		base = fmt.Sprintf("imported-%d", idx+1)
	}
	return base
}

func (s *Server) prepareImportedCredential(ctx context.Context, entry *createAccountReq) (*pool.Credential, error) {
	if err := validateImportBase(entry); err != nil {
		return nil, err
	}
	cred, err := credentialFromFile(entry.toCredentialFile())
	if err != nil {
		return nil, err
	}
	if importNeedsRefresh(entry) {
		if s.refresher == nil {
			return nil, fmt.Errorf("kiro_auth_token_raw.accessToken/profileArn missing; refreshToken validation requires a configured refresher")
		}
		if err := s.refresher.Refresh(ctx, cred); err != nil {
			return nil, fmt.Errorf("refreshToken validation failed: %w", err)
		}
		copyCredentialToCreateReq(entry, cred)
	}
	if err := validateCreateReq(entry); err != nil {
		return nil, err
	}
	return cred, nil
}

func validateImportBase(r *createAccountReq) error {
	if r.ID == "" {
		return fmt.Errorf("id is required")
	}
	if r.KiroAuthTokenRaw.RefreshToken == "" {
		return fmt.Errorf("kiro_auth_token_raw.refreshToken is required")
	}
	if r.MaxInFlight < 0 {
		return fmt.Errorf("max_in_flight must be >= 0")
	}
	if strings.EqualFold(strings.TrimSpace(r.KiroAuthTokenRaw.AuthMethod), "api_key") {
		return fmt.Errorf("kiro_auth_token_raw.authMethod api_key is not supported by the current Kiro pool")
	}
	return nil
}

func importNeedsRefresh(r *createAccountReq) bool {
	return r.KiroAuthTokenRaw.AccessToken == "" || r.KiroAuthTokenRaw.ProfileARN == ""
}

func copyCredentialToCreateReq(r *createAccountReq, cred *pool.Credential) {
	cred.Mu.RLock()
	defer cred.Mu.RUnlock()
	r.Provider = cred.Provider
	r.KiroAuthTokenRaw.AccessToken = cred.AccessToken
	r.KiroAuthTokenRaw.RefreshToken = cred.RefreshToken
	if cred.ExpiresAt > 0 {
		r.KiroAuthTokenRaw.ExpiresAt = time.Unix(cred.ExpiresAt, 0).UTC().Format(time.RFC3339)
	}
	r.KiroAuthTokenRaw.ProfileARN = cred.ProfileARN
	r.KiroAuthTokenRaw.Region = cred.Region
	r.KiroAuthTokenRaw.SSORegion = cred.SSORegion
	r.KiroAuthTokenRaw.ClientID = cred.ClientID
	r.KiroAuthTokenRaw.ClientSecret = cred.ClientSecret
	if cred.AuthType != "" {
		r.KiroAuthTokenRaw.AuthMethod = cred.AuthType
	}
}

func cloneCredentials(creds []*pool.Credential) []*pool.Credential {
	out := make([]*pool.Credential, 0, len(creds))
	for _, c := range creds {
		if cp := cloneCredential(c); cp != nil {
			out = append(out, cp)
		}
	}
	return out
}

func cloneCredential(c *pool.Credential) *pool.Credential {
	if c == nil {
		return nil
	}
	c.Mu.RLock()
	defer c.Mu.RUnlock()
	cp := &pool.Credential{
		ID:               c.ID,
		Label:            c.Label,
		Provider:         c.Provider,
		Priority:         c.Priority,
		Disabled:         c.Disabled,
		DisableCooling:   c.DisableCooling,
		MaxInFlight:      c.MaxInFlight,
		ProxyURL:         c.ProxyURL,
		Credentials:      c.Credentials,
		Quota:            c.Quota,
		Success:          c.Success,
		Failed:           c.Failed,
		InFlight:         c.InFlight,
		LastUsedAt:       c.LastUsedAt,
		DisabledReason:   c.DisabledReason,
		DisabledAt:       c.DisabledAt,
		LastQuotaAt:      c.LastQuotaAt,
		LastQuotaError:   c.LastQuotaError,
		LastQuotaErrorAt: c.LastQuotaErrorAt,
	}
	if c.LastQuota != nil {
		q := *c.LastQuota
		cp.LastQuota = &q
	}
	if len(c.ModelStates) > 0 {
		cp.ModelStates = make(map[string]*pool.ModelState, len(c.ModelStates))
		for k, v := range c.ModelStates {
			if v == nil {
				continue
			}
			cpV := *v
			cp.ModelStates[k] = &cpV
		}
	}
	if len(c.InFlightByModel) > 0 {
		cp.InFlightByModel = make(map[string]int64, len(c.InFlightByModel))
		for k, v := range c.InFlightByModel {
			cp.InFlightByModel[k] = v
		}
	}
	if len(c.Metadata) > 0 {
		cp.Metadata = make(map[string]string, len(c.Metadata))
		for k, v := range c.Metadata {
			cp.Metadata[k] = v
		}
	}
	return cp
}

// allowMutate returns true when the durable PostgreSQL account store is
// configured. When false, it has already written a 405 response to w.
func (s *Server) allowMutate(w http.ResponseWriter) bool {
	if s.credStore == nil {
		http.Error(w, "account store is not configured", http.StatusMethodNotAllowed)
		return false
	}
	return true
}

// persistCreds writes the scheduler's full credential set to PostgreSQL.
func (s *Server) persistCreds() error {
	if s.credStore == nil {
		return fmt.Errorf("account store is not configured")
	}
	return s.credStore.SaveAll(context.Background(), s.sched.All())
}

func (s *Server) persistOne(cred *pool.Credential) error {
	if s.credStore == nil {
		return fmt.Errorf("account store is not configured")
	}
	return s.credStore.SaveOne(context.Background(), cred)
}

func (s *Server) deletePersistedCred(ctx context.Context, id string) error {
	if s.credStore == nil {
		return fmt.Errorf("account store is not configured")
	}
	return s.credStore.Delete(ctx, id)
}

func validateCreateReq(r *createAccountReq) error {
	if r.ID == "" {
		return fmt.Errorf("id is required")
	}
	if r.KiroAuthTokenRaw.AccessToken == "" {
		return fmt.Errorf("kiro_auth_token_raw.accessToken is required")
	}
	if r.KiroAuthTokenRaw.RefreshToken == "" {
		return fmt.Errorf("kiro_auth_token_raw.refreshToken is required")
	}
	if r.KiroAuthTokenRaw.ProfileARN == "" {
		return fmt.Errorf("kiro_auth_token_raw.profileArn is required")
	}
	if r.MaxInFlight < 0 {
		return fmt.Errorf("max_in_flight must be >= 0")
	}
	return nil
}

// credentialFromFile builds a *pool.Credential from a CredentialFile so
// create/import endpoints share validation before writing PostgreSQL.
func credentialFromFile(f pool.CredentialFile) (*pool.Credential, error) {
	authType := strings.ToLower(strings.TrimSpace(f.KiroAuthTokenRaw.AuthMethod))
	switch authType {
	case "social", "":
		authType = "social"
	case "iam", "idc", "oidc":
		authType = "idc"
	default:
		authType = "social"
	}
	priority := f.Priority
	if priority == 0 {
		priority = 100
	}
	expiresAt := int64(0)
	if s := f.KiroAuthTokenRaw.ExpiresAt; s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return nil, fmt.Errorf("parse expiresAt: %w", err)
		}
		expiresAt = t.Unix()
	}
	provider := strings.ToLower(strings.TrimSpace(f.Provider))
	if provider == "" {
		provider = "kiro"
	}
	c := &pool.Credential{
		ID:             f.ID,
		Label:          f.Label,
		Provider:       provider,
		Priority:       priority,
		Disabled:       f.Disabled,
		DisableCooling: f.DisableCooling,
		MaxInFlight:    f.MaxInFlight,
		ProxyURL:       strings.TrimSpace(f.ProxyURL),
		Credentials: auth.Credentials{
			AccessToken:  f.KiroAuthTokenRaw.AccessToken,
			RefreshToken: f.KiroAuthTokenRaw.RefreshToken,
			ExpiresAt:    expiresAt,
			Region:       f.KiroAuthTokenRaw.Region,
			SSORegion:    f.KiroAuthTokenRaw.SSORegion,
			ClientID:     f.KiroAuthTokenRaw.ClientID,
			ClientSecret: f.KiroAuthTokenRaw.ClientSecret,
			ProfileARN:   f.KiroAuthTokenRaw.ProfileARN,
			AuthType:     authType,
		},
	}
	return c, nil
}

func errIsDuplicate(err error) bool {
	return err != nil && err.Error() == pool.ErrDuplicateID.Error()
}
