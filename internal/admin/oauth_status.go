// [fork] OAuth flow status tracking + manual-callback fallback. The
// browser polls /admin/oauth/status?state=X every 1s while the modal is
// open. The watcher goroutine updates the status as it advances; when
// the loopback can't reach the browser (e.g. remote admin behind a
// firewall) the user can paste the redirect URL into the modal and
// trigger /admin/oauth/manual_callback.

package admin

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/niuma/kirocc-pro/internal/pool"
	"github.com/niuma/kirocc-pro/internal/provider"
)

// OAuthStatus is the current state of an in-flight authorization flow.
type oauthStatus string

const (
	oauthPending oauthStatus = "pending"
	oauthSuccess oauthStatus = "success"
	oauthError   oauthStatus = "error"
	oauthExpired oauthStatus = "expired"
)

// oauthFlowEntry is the per-state record consulted by the polling
// endpoint AND by the manual_callback handler.
type oauthFlowEntry struct {
	State    string
	Provider provider.Provider
	Flow     *provider.OAuthFlow
	Extras   map[string]string
	Created  time.Time

	mu      sync.Mutex
	status  oauthStatus
	message string
	credID  string
	done    chan struct{} // closed when status becomes terminal
}

func (e *oauthFlowEntry) setStatus(s oauthStatus, msg, credID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	// Terminal states are final.
	if e.status == oauthSuccess || e.status == oauthError || e.status == oauthExpired {
		return
	}
	e.status = s
	e.message = msg
	e.credID = credID
	if s != oauthPending {
		select {
		case <-e.done:
		default:
			close(e.done)
		}
	}
}

func (e *oauthFlowEntry) snapshot() (oauthStatus, string, string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.status, e.message, e.credID
}

// oauthFlowRegistry tracks in-flight flows keyed by state. The registry
// is per-Server; entries auto-expire after OAuth flow timeout.
type oauthFlowRegistry struct {
	mu  sync.Mutex
	all map[string]*oauthFlowEntry
}

func newOAuthFlowRegistry() *oauthFlowRegistry {
	return &oauthFlowRegistry{all: make(map[string]*oauthFlowEntry)}
}

func (r *oauthFlowRegistry) put(e *oauthFlowEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.all[e.State] = e
}

func (r *oauthFlowRegistry) get(state string) *oauthFlowEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.all[state]
}

func (r *oauthFlowRegistry) remove(state string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.all, state)
}

func (r *oauthFlowRegistry) sweep() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	removed := 0
	for k, e := range r.all {
		if now.Sub(e.Created) > 2*oauthFlowTimeout {
			delete(r.all, k)
			removed++
		}
	}
	return removed
}

// --- HTTP handlers --------------------------------------------------

type oauthStatusResp struct {
	State        string `json:"state"`
	Status       string `json:"status"`
	Message      string `json:"message,omitempty"`
	CredentialID string `json:"credential_id,omitempty"`
}

func (s *Server) handleOAuthStatus(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	if state == "" {
		http.Error(w, "missing state", http.StatusBadRequest)
		return
	}
	if s.oauthFlows == nil {
		writeJSON(w, http.StatusOK, oauthStatusResp{State: state, Status: string(oauthExpired)})
		return
	}
	e := s.oauthFlows.get(state)
	if e == nil {
		writeJSON(w, http.StatusOK, oauthStatusResp{State: state, Status: string(oauthExpired)})
		return
	}
	status, msg, credID := e.snapshot()
	writeJSON(w, http.StatusOK, oauthStatusResp{
		State: state, Status: string(status), Message: msg, CredentialID: credID,
	})
}

type oauthManualCallbackReq struct {
	State       string `json:"state"`
	CallbackURL string `json:"callback_url"`
}

// handleOAuthManualCallback accepts a callback URL the user copied from
// their browser. Useful when the loopback can't catch the redirect
// (remote admin without port forwarding, popup blocked, etc.). We parse
// the URL, validate state against the registry, and feed the params to
// the existing CompleteOAuth path — same code as the loopback watcher
// uses.
func (s *Server) handleOAuthManualCallback(w http.ResponseWriter, r *http.Request) {
	if !s.allowMutate(w) {
		return
	}
	var body oauthManualCallbackReq
	if err := json.UnmarshalRead(r.Body, &body); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.State == "" || body.CallbackURL == "" {
		http.Error(w, "state and callback_url required", http.StatusBadRequest)
		return
	}
	if s.oauthFlows == nil {
		http.Error(w, "no in-flight OAuth flows", http.StatusServiceUnavailable)
		return
	}
	e := s.oauthFlows.get(body.State)
	if e == nil {
		http.Error(w, "unknown state (expired?)", http.StatusNotFound)
		return
	}
	u, err := url.Parse(strings.TrimSpace(body.CallbackURL))
	if err != nil {
		http.Error(w, "invalid callback_url: "+err.Error(), http.StatusBadRequest)
		return
	}
	params := u.Query()
	if params.Get("state") != body.State {
		http.Error(w, "callback state does not match request state", http.StatusBadRequest)
		return
	}
	// [fork] Seed the loopback's capturedPath from the pasted URL so
	// CompleteOAuth can reconstruct the redirect_uri exactly as Kiro
	// expects at token-exchange (path is provider-chosen, e.g.
	// "/oauth/callback" for GitHub social, "/oauth/kiro/idc/callback"
	// for IDC). Without this the manual flow drops the path and Kiro
	// returns 400 "Bad request".
	if e.Flow != nil && e.Flow.Loopback != nil && u.Path != "" {
		e.Flow.Loopback.SetCapturedPath(u.Path)
	}
	// Complete the flow synchronously. The loopback watcher (if still
	// running) will harmlessly time out — closing the loopback here
	// would race with it, so we let setStatus mark this entry done and
	// let the watcher exit naturally.
	s.completeOAuthFromParams(r.Context(), e, params)
	status, msg, credID := e.snapshot()
	writeJSON(w, http.StatusOK, oauthStatusResp{
		State: body.State, Status: string(status), Message: msg, CredentialID: credID,
	})
}

// completeOAuthFromParams runs the same complete + register + persist
// pipeline as the loopback watcher, but driven by an externally supplied
// params map. Safe to call from both paths because oauthFlowEntry.
// setStatus is a no-op after the first terminal state.
func (s *Server) completeOAuthFromParams(ctx context.Context, e *oauthFlowEntry, params url.Values) {
	cred, err := e.Provider.CompleteOAuth(ctx, params, e.Flow, e.Extras)
	if err != nil {
		e.setStatus(oauthError, err.Error(), "")
		return
	}
	if err := s.sched.Add(cred); err != nil {
		if errors.Is(err, pool.ErrDuplicateID) {
			e.setStatus(oauthError, "duplicate credential id: "+cred.ID, "")
			return
		}
		e.setStatus(oauthError, "scheduler add: "+err.Error(), "")
		return
	}
	if s.credsPath != "" {
		if err := s.persistCreds(); err != nil {
			_ = s.sched.Remove(cred.ID)
			e.setStatus(oauthError, "persist: "+err.Error(), "")
			return
		}
	}
	e.setStatus(oauthSuccess, "", cred.ID)
}
