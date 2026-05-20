package pool

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/niuma/kirocc-pro/internal/auth"
)

// singleAccountConductor wraps an *auth.AuthManager as a Conductor with a
// pool of exactly one Credential. The credential's embedded auth.Credentials
// is refreshed in place on every Acquire via authMgr.GetToken so the
// upstream single-account auto-refresh path keeps working for users who
// have not opted into the multi-account JSON pool.
type singleAccountConductor struct {
	authMgr *auth.AuthManager
	cred    *Credential
}

// SingleAccountID is the synthetic Credential ID used for the single-account
// fallback path. Exposed so the admin API can render a recognizable label.
const SingleAccountID = "default"

// NewSingleAccount returns a Scheduler and Conductor backed by a single
// Credential whose tokens come from authMgr. Use this when CredsJSON is
// empty. The Scheduler is a DefaultScheduler containing exactly one entry
// so the admin endpoints can still list/inspect quota and counters for it.
func NewSingleAccount(authMgr *auth.AuthManager) (Scheduler, Conductor, error) {
	if authMgr == nil {
		return nil, nil, errors.New("pool: nil auth manager")
	}
	cred := &Credential{
		ID:       SingleAccountID,
		Label:    "default (sqlite)",
		Priority: 100,
	}
	s := NewDefaultScheduler()
	s.Register([]*Credential{cred})
	return s, &singleAccountConductor{authMgr: authMgr, cred: cred}, nil
}

// Acquire refreshes the embedded auth.Credentials via authMgr.GetToken and
// returns the single Credential. model and sessionID are accepted to satisfy
// the Conductor interface but are unused in this mode (one cred only, so
// affinity is trivially satisfied).
func (c *singleAccountConductor) Acquire(ctx context.Context, model, sessionID string) (*Credential, error) {
	creds, err := c.authMgr.GetToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("pool: single-account auth: %w", err)
	}
	c.cred.Mu.Lock()
	c.cred.Credentials = *creds
	c.cred.LastUsedAt = time.Now()
	c.cred.Mu.Unlock()
	return c.cred, nil
}

// Release is a no-op for the single-account conductor. Counter updates are
// performed by the Scheduler's MarkSuccess / MarkRateLimit / MarkAuthError.
func (c *singleAccountConductor) Release(_ *Credential) {}

// NewSelector returns a Selector for the given strategy name. Recognized
// values: "round-robin" (default), "fill-first", "least-used". An empty or
// unknown string returns RoundRobinSelector.
func NewSelector(strategy string) Selector {
	switch strategy {
	case "fill-first":
		return &FillFirstSelector{}
	case "least-used":
		return &LeastUsedSelector{}
	default:
		return &RoundRobinSelector{}
	}
}
