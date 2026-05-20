package pool

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/niuma/kirocc-pro/internal/auth"
)

// RefreshSkew is how far before expiry a credential is considered "stale"
// and triggers a preemptive refresh on Acquire.
const RefreshSkew = 5 * time.Minute

// Refresher knows how to refresh a single credential's OAuth tokens in
// place. Used by Conductor.Acquire to preempt token expiry in
// multi-account mode (single-account mode handles refresh via AuthManager
// internally, so it leaves the Conductor's refresher unset).
type Refresher interface {
	// ShouldRefresh reports whether cred is within the refresh skew of
	// expiry (or already expired) and has the material needed to refresh.
	ShouldRefresh(cred *Credential) bool

	// Refresh updates cred.Credentials in place. Returns nil on success;
	// on error the in-memory credential is unchanged.
	Refresh(ctx context.Context, cred *Credential) error
}

// JSONFileRefresher refreshes credentials via auth.RefreshTokens and
// atomically persists the entire pool back to a JSON file after each
// successful refresh.
//
// SavePath may be empty: the refresh still updates the in-memory cred, but
// the new tokens are not written to disk (they will be lost on restart).
//
// AllCreds must return the full pool snapshot used by SaveToJSON; the
// closure lets the refresher avoid a hard dependency on Scheduler.
type JSONFileRefresher struct {
	httpClient *http.Client
	savePath   string
	allCreds   func() []*Credential

	saveMu sync.Mutex // serializes disk writes across concurrent refreshes
}

// NewJSONFileRefresher constructs a Refresher persisting to savePath.
// Pass httpClient = nil to use a default 30s-timeout client.
func NewJSONFileRefresher(savePath string, allCreds func() []*Credential, httpClient *http.Client) *JSONFileRefresher {
	return &JSONFileRefresher{
		httpClient: httpClient,
		savePath:   savePath,
		allCreds:   allCreds,
	}
}

// ShouldRefresh implements Refresher.
func (r *JSONFileRefresher) ShouldRefresh(cred *Credential) bool {
	cred.Mu.RLock()
	exp := cred.ExpiresAt
	hasRefresh := cred.RefreshToken != ""
	cred.Mu.RUnlock()
	if !hasRefresh || exp == 0 {
		return false
	}
	return time.Until(time.Unix(exp, 0)) < RefreshSkew
}

// Refresh implements Refresher.
func (r *JSONFileRefresher) Refresh(ctx context.Context, cred *Credential) error {
	cred.Mu.RLock()
	snap := cred.Credentials
	cred.Mu.RUnlock()

	refreshed, err := auth.RefreshTokens(ctx, &snap, r.httpClient)
	if err != nil {
		return err
	}

	cred.Mu.Lock()
	cred.Credentials = *refreshed
	cred.Mu.Unlock()

	if r.savePath != "" && r.allCreds != nil {
		r.saveMu.Lock()
		defer r.saveMu.Unlock()
		if err := SaveToJSON(r.savePath, r.allCreds()); err != nil {
			// Non-fatal: in-memory tokens are still valid for this run.
			slog.WarnContext(ctx, "pool: persist refreshed creds failed",
				"path", r.savePath, "cred_id", cred.ID, "err", err)
		}
	}
	return nil
}
