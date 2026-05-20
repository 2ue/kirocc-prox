package admin

import (
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/niuma/kirocc-pro/internal/settings"
)

// settingsRequiredErr is returned by /admin/settings/* when no
// settings store is wired (single-account legacy mode).
var settingsRequiredErr = "admin: settings store not configured (start with -settings to enable)"

// serverInfoBlock is the server-side runtime view injected into GET
// /admin/settings. These fields are read-only (set at startup via flags
// or env vars); the UI shows them so operators don't have to ssh in to
// check what's bound.
type serverInfoBlock struct {
	Host          string         `json:"host"`
	Port          int            `json:"port"`
	AdminHost     string         `json:"admin_host"`
	AdminPort     int            `json:"admin_port"`
	TLSEnabled    bool           `json:"tls_enabled"`
	PublicBaseURL string         `json:"public_base_url,omitempty"`
	ProxyBaseURL  string         `json:"proxy_base_url,omitempty"`
	CredsPath     string         `json:"creds_path,omitempty"`
	MultiAccount  bool           `json:"multi_account"`
	GeoIP         geoIPInfoBlock `json:"geoip"`
}

// geoIPInfoBlock surfaces the MMDB metadata so the UI can show
// "GeoLite2-Country loaded (build 2026-04-01)" or "GeoIP disabled".
type geoIPInfoBlock struct {
	Loaded     bool   `json:"loaded"`
	Path       string `json:"path,omitempty"`
	DBType     string `json:"db_type,omitempty"`
	BuildEpoch int64  `json:"build_epoch,omitempty"`
	Nodes      uint64 `json:"nodes,omitempty"`
}

func (s *Server) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	if s.settings == nil {
		http.Error(w, settingsRequiredErr, http.StatusServiceUnavailable)
		return
	}
	cur := s.settings.Get()
	masked := make([]settings.APIKey, len(cur.APIKeys))
	for i, k := range cur.APIKeys {
		masked[i] = settings.APIKey{
			ID:        k.ID,
			Label:     k.Label,
			Key:       settings.MaskAPIKey(k.Key),
			Enabled:   k.Enabled,
			CreatedAt: k.CreatedAt,
		}
	}
	// Envelope merges the user-mutable settings with the runtime server view.
	// Using a map keeps the existing JSON shape for the settings sections
	// and adds the new `server` block alongside.
	resp := map[string]any{
		"remote_management": cur.RemoteManagement,
		"api_keys":          masked,
		"system":            cur.System,
		"network":           cur.Network,
		"streaming":         cur.Streaming,
		"optimizations":     cur.Optimizations,
		"schema_version":    cur.SchemaVersion,
		"server": s.buildServerInfo(),
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleSettingsPut accepts a partial JSON body and merges it into
// the current settings. Sections present in the body replace the
// in-memory section verbatim. The api_keys field is IGNORED here —
// use /admin/api-keys for those mutations so we don't accidentally
// overwrite key secrets via a partial round-trip.
func (s *Server) handleSettingsPut(w http.ResponseWriter, r *http.Request) {
	if s.settings == nil {
		http.Error(w, settingsRequiredErr, http.StatusServiceUnavailable)
		return
	}
	var patch struct {
		RemoteManagement *settings.RemoteManagement `json:"remote_management,omitempty"`
		System           *settings.System           `json:"system,omitempty"`
		Network          *settings.Network          `json:"network,omitempty"`
		Streaming        *settings.Streaming        `json:"streaming,omitempty"`
		Optimizations    *settings.Optimizations    `json:"optimizations,omitempty"`
	}
	if err := json.UnmarshalRead(r.Body, &patch); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	out, err := s.settings.Update(func(c *settings.Settings) error {
		if patch.RemoteManagement != nil {
			c.RemoteManagement = *patch.RemoteManagement
		}
		if patch.System != nil {
			c.System = *patch.System
		}
		if patch.Network != nil {
			// Preserve api_keys regardless of body.
			c.Network = *patch.Network
		}
		if patch.Streaming != nil {
			c.Streaming = *patch.Streaming
		}
		if patch.Optimizations != nil {
			c.Optimizations = *patch.Optimizations
		}
		return nil
	})
	if err != nil {
		http.Error(w, "update failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// --- API key endpoints ---------------------------------------------

func (s *Server) handleAPIKeysList(w http.ResponseWriter, r *http.Request) {
	if s.settings == nil {
		http.Error(w, settingsRequiredErr, http.StatusServiceUnavailable)
		return
	}
	cur := s.settings.Get()
	out := make([]map[string]any, 0, len(cur.APIKeys))
	for _, k := range cur.APIKeys {
		out = append(out, map[string]any{
			"id":          k.ID,
			"label":       k.Label,
			"key_masked":  settings.MaskAPIKey(k.Key),
			"enabled":     k.Enabled,
			"created_at":  k.CreatedAt,
			"expires_at":  k.ExpiresAt,
			"quota_limit": k.QuotaLimit,
			"used_tokens": k.UsedTokens,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": out})
}

func (s *Server) handleAPIKeysCreate(w http.ResponseWriter, r *http.Request) {
	if s.settings == nil {
		http.Error(w, settingsRequiredErr, http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Label      string `json:"label"`
		Key        string `json:"key"`         // optional; auto-generate when empty
		ExpiresAt  int64  `json:"expires_at"`  // 0 = never expires
		QuotaLimit int64  `json:"quota_limit"` // 0 = unlimited
	}
	if err := json.UnmarshalRead(r.Body, &body); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	k, err := s.settings.AddAPIKey(settings.APIKeyOptions{
		Label:      body.Label,
		KeyValue:   body.Key,
		ExpiresAt:  body.ExpiresAt,
		QuotaLimit: body.QuotaLimit,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Reveal the secret value in the response — only time the UI
	// gets to see it. Subsequent GETs return the masked form.
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":          k.ID,
		"label":       k.Label,
		"key":         k.Key,
		"key_masked":  settings.MaskAPIKey(k.Key),
		"enabled":     k.Enabled,
		"created_at":  k.CreatedAt,
		"expires_at":  k.ExpiresAt,
		"quota_limit": k.QuotaLimit,
		"used_tokens": k.UsedTokens,
	})
}

func (s *Server) handleAPIKeyUpdate(w http.ResponseWriter, r *http.Request) {
	if s.settings == nil {
		http.Error(w, settingsRequiredErr, http.StatusServiceUnavailable)
		return
	}
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	var body struct {
		Label      string `json:"label,omitempty"`
		Enabled    *bool  `json:"enabled,omitempty"`
		ExpiresAt  *int64 `json:"expires_at,omitempty"`
		QuotaLimit *int64 `json:"quota_limit,omitempty"`
	}
	if err := json.UnmarshalRead(r.Body, &body); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.settings.UpdateAPIKey(id, settings.APIKeyPatch{
		Label:      body.Label,
		Enabled:    body.Enabled,
		ExpiresAt:  body.ExpiresAt,
		QuotaLimit: body.QuotaLimit,
	}); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, settings.ErrAPIKeyNotFound) {
			status = http.StatusNotFound
		}
		http.Error(w, err.Error(), status)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAPIKeyRotate(w http.ResponseWriter, r *http.Request) {
	if s.settings == nil {
		http.Error(w, settingsRequiredErr, http.StatusServiceUnavailable)
		return
	}
	id := r.PathValue("id")
	newVal, err := s.settings.RotateAPIKey(id)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, settings.ErrAPIKeyNotFound) {
			status = http.StatusNotFound
		}
		http.Error(w, err.Error(), status)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":         id,
		"key":        newVal,
		"key_masked": settings.MaskAPIKey(newVal),
	})
}

func (s *Server) handleAPIKeyDelete(w http.ResponseWriter, r *http.Request) {
	if s.settings == nil {
		http.Error(w, settingsRequiredErr, http.StatusServiceUnavailable)
		return
	}
	id := r.PathValue("id")
	if err := s.settings.DeleteAPIKey(id); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, settings.ErrAPIKeyNotFound) {
			status = http.StatusNotFound
		}
		http.Error(w, err.Error(), status)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// buildServerInfo composes the read-only server block. Proxy host/port come
// from the proxy base URL (set in main via SetProxyConfig); if that URL
// can't be parsed we fall back to empty values rather than serving garbage.
func (s *Server) buildServerInfo() serverInfoBlock {
	var proxyHost string
	var proxyPort int
	if s.proxyBaseURL != "" {
		if u, err := url.Parse(s.proxyBaseURL); err == nil {
			proxyHost = u.Hostname()
			if p := u.Port(); p != "" {
				proxyPort, _ = strconv.Atoi(p)
			}
		}
	}
	geo := geoIPInfoBlock{}
	if s.geoResolver != nil {
		st := s.geoResolver.Status()
		geo = geoIPInfoBlock{
			Loaded:     st.Loaded,
			Path:       st.Path,
			DBType:     st.DBType,
			BuildEpoch: st.BuildEpoch,
			Nodes:      st.Nodes,
		}
	}
	return serverInfoBlock{
		Host:          proxyHost,
		Port:          proxyPort,
		AdminHost:     s.host,
		AdminPort:     s.port,
		TLSEnabled:    s.tlsCert != "" && s.tlsKey != "",
		PublicBaseURL: s.publicBaseURL,
		ProxyBaseURL:  s.proxyBaseURL,
		CredsPath:     s.credsPath,
		MultiAccount:  s.credsPath != "",
		GeoIP:         geo,
	}
}
