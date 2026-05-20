package pool

import (
	"context"
	"log/slog"
	"time"
)

// DefaultConductor implements Conductor with session affinity. See the
// Conductor interface doc for sticky-session semantics.
type DefaultConductor struct {
	sched     Scheduler
	selector  Selector
	affinity  *Affinity
	refresher Refresher // optional; nil = no preemptive token refresh
}

// NewConductor constructs a DefaultConductor. The affinity argument may be nil
// to disable session stickiness entirely. Refresher may be attached after
// construction via SetRefresher.
func NewConductor(s Scheduler, sel Selector, aff *Affinity) *DefaultConductor {
	return &DefaultConductor{
		sched:    s,
		selector: sel,
		affinity: aff,
	}
}

// SetRefresher attaches a token refresher invoked from Acquire when the
// selected credential is within RefreshSkew of expiry. Passing nil disables
// the preemptive refresh path.
func (d *DefaultConductor) SetRefresher(r Refresher) {
	d.refresher = r
}

// Acquire returns a credential for the given (model, sessionID). If the
// session has a live affinity binding and the bound credential is ready, that
// credential is reused. Otherwise the configured selector picks one; if the
// session had no prior binding, the freshly-picked credential is bound.
func (d *DefaultConductor) Acquire(ctx context.Context, model, sessionID string) (*Credential, error) {
	hadBinding := false
	if d.affinity != nil && sessionID != "" {
		if id, ok := d.affinity.Get(sessionID); ok {
			hadBinding = true
			if c := d.sched.Lookup(id); c != nil && c.IsReady() {
				return c, nil
			}
			// Bound cred is gone or in cooldown: fall through to selector,
			// but do NOT rewrite affinity (sticky-on-recovery semantics).
		}
	}

	ready := d.sched.Ready()
	if hint := RegionHintFrom(ctx); hint != "" {
		ready = FilterByRegion(ready, hint)
	}
	c, err := d.selector.Pick(ready, model)
	if err != nil {
		return nil, err
	}

	if d.affinity != nil && sessionID != "" && !hadBinding {
		d.affinity.Set(sessionID, c.ID)
	}
	d.maybeRefresh(ctx, c)
	return c, nil
}

// maybeRefresh runs the preemptive refresh if a Refresher is attached AND the
// credential is within RefreshSkew of expiry. Refresh failures are logged and
// swallowed: the upstream call will still happen with the (possibly stale)
// token, and the next 403 marks the credential through the normal path.
func (d *DefaultConductor) maybeRefresh(ctx context.Context, c *Credential) {
	if d.refresher == nil || !d.refresher.ShouldRefresh(c) {
		return
	}
	if err := d.refresher.Refresh(ctx, c); err != nil {
		slog.WarnContext(ctx, "pool: preemptive refresh failed, using stale token",
			"cred_id", c.ID, "err", err)
	}
}

// Release marks the credential as recently used. Future versions may also
// decrement an in-flight counter here.
func (d *DefaultConductor) Release(cred *Credential) {
	if cred == nil {
		return
	}
	cred.Mu.Lock()
	cred.LastUsedAt = time.Now()
	cred.Mu.Unlock()
}
