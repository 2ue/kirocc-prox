// [fork] /admin/config/cc-switch returns the configuration cc-switch (or
// any Claude-Code-compatible config tool) needs to point Claude Code at
// this proxy: base URL, optional API key, list of advertised models.
//
// cc-switch can poll this endpoint to auto-discover proxy availability;
// kirocc-pro doesn't validate cc-switch is the caller, only that the
// admin-auth middleware allowed the request through.

package admin

import (
	"net/http"

	"github.com/niuma/kirocc-pro/internal/models"
)

type ccSwitchResp struct {
	Name     string   `json:"name"`
	BaseURL  string   `json:"base_url"`
	APIKey   string   `json:"api_key"`
	Models   []string `json:"models"`
	HelpHint string   `json:"help_hint"`
}

func (s *Server) handleCCSwitchConfig(w http.ResponseWriter, r *http.Request) {
	base := s.proxyBaseURL
	if base == "" {
		// Best-effort guess from the inbound Host. The admin and proxy
		// share a host but different ports; we can't know the proxy
		// port from the admin's request, so leave empty when not set.
		base = ""
	}
	resp := ccSwitchResp{
		Name:     "kirocc-pro",
		BaseURL:  base,
		APIKey:   s.proxyAPIKey,
		Models:   advertisedModels(s),
		HelpHint: "在 Claude Code 中设置 ANTHROPIC_BASE_URL=<base_url> 和 ANTHROPIC_AUTH_TOKEN=<api_key>",
	}
	writeJSON(w, http.StatusOK, resp)
}

// advertisedModels returns the canonical list of model names kirocc-pro
// can route. Sourced from models.ListModels() so it stays in sync with
// the rest of the proxy.
func advertisedModels(_ *Server) []string {
	return models.ListModels()
}
