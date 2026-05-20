// [fork] OAuth start endpoint. The flow:
//
//   1. Browser hits POST /admin/oauth/start?provider=kiro|codex.
//   2. Server calls Provider.StartOAuth which:
//        - Picks a free loopback port the provider's auth server accepts
//        - Starts a temporary HTTP listener on that port
//        - Builds the authorization URL with redirect_uri pointing AT that
//          loopback (NOT at the admin server — providers whitelist
//          specific localhost URIs)
//      Returns auth_url + state.
//   3. Server kicks off a watcher goroutine that:
//        - Waits up to 10 minutes for the loopback to capture a callback
//        - Validates state, calls Provider.CompleteOAuth(params, flow)
//        - Registers the resulting *pool.Credential into the pool
//        - Atomically writes the JSON pool file
//        - Logs progress to the in-memory event log
//   4. Browser opens auth_url in a new tab. The user logs in; the
//      provider redirects to http://localhost:PORT/... which hits the
//      loopback directly (NOT through the admin server). The loopback
//      shows a static "you can close this tab" landing page.
//   5. The user closes the auth tab and returns to /admin#/accounts. The
//      next auto-refresh shows the new credential.
//
// Critical limitation: this only works when the admin server and the
// user's browser are on the SAME machine. For remote admin deployments,
// the user must SSH port-forward the loopback ports to reach the
// browser side, OR import the credential manually.

package admin

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"time"
)

const oauthFlowTimeout = 10 * time.Minute

type oauthStartReq struct {
	Provider string            `json:"provider"`
	Extras   map[string]string `json:"extras,omitempty"`
}

type oauthStartResp struct {
	AuthURL    string `json:"auth_url"`
	State      string `json:"state"`
	LoopbackOn int    `json:"loopback_port"`
}

func (s *Server) handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	if !s.allowMutate(w) {
		return
	}
	if s.registry == nil {
		http.Error(w, "no providers registered", http.StatusServiceUnavailable)
		return
	}
	var body oauthStartReq
	if err := json.UnmarshalRead(r.Body, &body); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.Provider == "" {
		body.Provider = "kiro"
	}
	p, err := s.registry.Get(body.Provider)
	if err != nil {
		http.Error(w, "unknown provider: "+body.Provider, http.StatusBadRequest)
		return
	}
	if !p.SupportsOAuth() {
		http.Error(w, "provider does not support OAuth: "+body.Provider, http.StatusBadRequest)
		return
	}

	flow, err := p.StartOAuth(r.Context(), body.Extras)
	if err != nil {
		slog.Error("admin: oauth start failed", "provider", body.Provider, "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	entry := &oauthFlowEntry{
		State:    flow.State,
		Provider: p,
		Flow:     flow,
		Extras:   body.Extras,
		Created:  time.Now(),
		status:   oauthPending,
		done:     make(chan struct{}),
	}
	s.oauthFlows.put(entry)

	// Detach watcher from the request context so it survives after the
	// caller's HTTP response. The watcher races with the manual-callback
	// path: whichever finishes first calls setStatus to terminal; the
	// other becomes a no-op.
	go s.runOAuthWatcher(entry)

	writeJSON(w, http.StatusOK, oauthStartResp{
		AuthURL:    flow.AuthURL,
		State:      flow.State,
		LoopbackOn: flow.Loopback.Port(),
	})
}

func (s *Server) runOAuthWatcher(entry *oauthFlowEntry) {
	defer entry.Flow.Loopback.Close()

	ctx, cancel := context.WithTimeout(context.Background(), oauthFlowTimeout)
	defer cancel()

	// Wait for EITHER the loopback to capture a callback OR a manual
	// callback to flip the status terminal.
	captureCh := make(chan map[string][]string, 1)
	go func() {
		params, err := entry.Flow.Loopback.WaitContext(ctx, oauthFlowTimeout)
		if err == nil {
			captureCh <- params
		}
		close(captureCh)
	}()

	select {
	case params, ok := <-captureCh:
		if !ok {
			entry.setStatus(oauthExpired, "loopback timed out (10 min)", "")
			slog.Warn("admin: oauth: loopback timed out", "provider", entry.Provider.ID())
			return
		}
		s.completeOAuthFromParams(ctx, entry, params)
	case <-entry.done:
		// Manual callback already finished the flow; nothing more to do.
		return
	}
}
