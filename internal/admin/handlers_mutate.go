// [fork] New file added in fork. Implements the account-mutation endpoints
// (create / import / delete) used by the admin dashboard's Accounts page.
// All mutations refuse with 405 when the server was launched without a JSON
// credentials path (single-account mode is operator-only).

package admin

import (
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/niuma/kirocc-pro/internal/auth"
	"github.com/niuma/kirocc-pro/internal/pool"
)

// createAccountReq matches the on-disk pool.CredentialFile shape but is
// duplicated here so the HTTP layer is decoupled from any future renames.
type createAccountReq struct {
	ID               string                `json:"id"`
	Label            string                `json:"label,omitempty"`
	Priority         int                   `json:"priority,omitempty"`
	Disabled         bool                  `json:"disabled,omitempty"`
	DisableCooling   bool                  `json:"disable_cooling,omitempty"`
	ProxyURL         string                `json:"proxy_url,omitempty"`
	KiroAuthTokenRaw pool.KiroAuthTokenRaw `json:"kiro_auth_token_raw"`
}

func (r *createAccountReq) toCredentialFile() pool.CredentialFile {
	return pool.CredentialFile{
		ID:               r.ID,
		Label:            r.Label,
		Priority:         r.Priority,
		Disabled:         r.Disabled,
		DisableCooling:   r.DisableCooling,
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
// updated set back to the JSON file.
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
	if err := s.persistCreds(); err != nil {
		// Roll back the in-memory add to keep file and pool consistent.
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
	Label    *string `json:"label,omitempty"`
	Priority *int    `json:"priority,omitempty"`
	ProxyURL *string `json:"proxy_url,omitempty"`
}

// handleAccountPatch updates the mutable, non-secret fields on an
// existing credential (label, priority, proxy_url) and persists the
// pool back to disk. To rotate tokens, use POST /admin/accounts/{id}/refresh.
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
	c.Mu.Lock()
	if body.Label != nil {
		c.Label = *body.Label
	}
	if body.Priority != nil {
		c.Priority = *body.Priority
	}
	if body.ProxyURL != nil {
		c.ProxyURL = strings.TrimSpace(*body.ProxyURL)
	}
	c.Mu.Unlock()
	if err := s.persistCreds(); err != nil {
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
	if err := s.sched.Remove(id); err != nil {
		http.Error(w, "account not found", http.StatusNotFound)
		return
	}
	if err := s.persistCreds(); err != nil {
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
	if len(body.Accounts) == 0 {
		http.Error(w, "accounts array is empty", http.StatusBadRequest)
		return
	}

	resp := importResp{}
	mode := strings.ToLower(body.Mode)
	if mode == "" {
		mode = "append"
	}
	if mode != "append" && mode != "replace" {
		http.Error(w, "mode must be 'append' or 'replace'", http.StatusBadRequest)
		return
	}

	if mode == "replace" {
		incoming := make(map[string]struct{}, len(body.Accounts))
		for _, a := range body.Accounts {
			if a.ID != "" {
				incoming[a.ID] = struct{}{}
			}
		}
		for _, c := range s.sched.All() {
			if _, keep := incoming[c.ID]; !keep {
				if err := s.sched.Remove(c.ID); err == nil {
					resp.Removed++
				}
			}
		}
	}

	for i := range body.Accounts {
		entry := &body.Accounts[i]
		if err := validateCreateReq(entry); err != nil {
			resp.Errors = append(resp.Errors, fmt.Sprintf("[%d] %s: %v", i, entry.ID, err))
			continue
		}
		cred, err := credentialFromFile(entry.toCredentialFile())
		if err != nil {
			resp.Errors = append(resp.Errors, fmt.Sprintf("[%d] %s: %v", i, entry.ID, err))
			continue
		}
		if err := s.sched.Add(cred); err != nil {
			if errIsDuplicate(err) {
				resp.Skipped++
				continue
			}
			resp.Errors = append(resp.Errors, fmt.Sprintf("[%d] %s: %v", i, entry.ID, err))
			continue
		}
		resp.Added++
	}

	if err := s.persistCreds(); err != nil {
		slog.Error("admin: persist after import failed", "err", err)
		resp.Errors = append(resp.Errors, "persist: "+err.Error())
	}
	writeJSON(w, http.StatusOK, resp)
}

// decodeImport reads either {"accounts": [...]} or a raw array.
func decodeImport(r *http.Request) (importReq, error) {
	var raw any
	if err := json.UnmarshalRead(r.Body, &raw); err != nil {
		return importReq{}, fmt.Errorf("invalid JSON: %w", err)
	}
	// Re-marshal and try the envelope shape; if "accounts" is missing,
	// reinterpret the original as the array form.
	buf, err := json.Marshal(raw)
	if err != nil {
		return importReq{}, fmt.Errorf("re-marshal: %w", err)
	}
	var env importReq
	if err := json.Unmarshal(buf, &env); err == nil && len(env.Accounts) > 0 {
		return env, nil
	}
	var arr []createAccountReq
	if err := json.Unmarshal(buf, &arr); err != nil {
		return importReq{}, fmt.Errorf("expected an object {\"accounts\": [...]} or an array, got %T", raw)
	}
	return importReq{Accounts: arr, Mode: "append"}, nil
}

// allowMutate returns true if the server is in multi-account mode and is
// allowed to write back the JSON file. When false, it has already written
// a 405 response to w.
func (s *Server) allowMutate(w http.ResponseWriter) bool {
	if s.credsPath == "" {
		http.Error(w, "single-account mode: mutations require -creds-json", http.StatusMethodNotAllowed)
		return false
	}
	return true
}

// persistCreds atomically writes the scheduler's full set of credentials
// back to s.credsPath. Concurrent calls are serialized externally by the
// HTTP handler chain (one request at a time touches the scheduler set).
func (s *Server) persistCreds() error {
	return pool.SaveToJSON(s.credsPath, s.sched.All())
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
	return nil
}

// credentialFromFile builds a *pool.Credential from a CredentialFile,
// mirroring what pool.LoadFromJSON does for one entry. We re-implement here
// to avoid exposing the internal helper.
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
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			expiresAt = t.Unix()
		}
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
