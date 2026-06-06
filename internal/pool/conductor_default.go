package pool

import (
	"context"
	"log/slog"
	"time"
)

// DefaultConductor implements Conductor with session affinity. See the
// Conductor interface doc for sticky-session semantics.
type DefaultConductor struct {
	sched        Scheduler
	selector     Selector
	affinity     *Affinity
	refresher    Refresher // optional; nil = no preemptive token refresh
	runtime      RuntimeStateStore
	leaseTTL     time.Duration
	reservations *reservationTracker
}

// NewConductor constructs a DefaultConductor. The affinity argument may be nil
// to disable session stickiness entirely. Refresher may be attached after
// construction via SetRefresher.
func NewConductor(s Scheduler, sel Selector, aff *Affinity) *DefaultConductor {
	return &DefaultConductor{
		sched:        s,
		selector:     sel,
		affinity:     aff,
		leaseTTL:     10 * time.Minute,
		reservations: newReservationTracker(),
	}
}

// SetRefresher attaches a token refresher invoked from Acquire when the
// selected credential is within RefreshSkew of expiry. Passing nil disables
// the preemptive refresh path.
func (d *DefaultConductor) SetRefresher(r Refresher) {
	d.refresher = r
}

// SetRuntimeState attaches the Redis-backed runtime state coordinator used
// for distributed in-flight reservations, cooldown checks and affinity.
func (d *DefaultConductor) SetRuntimeState(r RuntimeStateStore, leaseTTL time.Duration) {
	d.runtime = r
	if leaseTTL > 0 {
		d.leaseTTL = leaseTTL
	}
	if d.reservations == nil {
		d.reservations = newReservationTracker()
	}
}

// RunAffinityJanitor periodically sweeps expired sticky-session bindings.
func (d *DefaultConductor) RunAffinityJanitor(ctx context.Context, interval time.Duration) {
	if d == nil || d.affinity == nil {
		return
	}
	d.affinity.RunJanitor(ctx, interval)
}

// Acquire returns a credential for the given (model, sessionID). If the
// session has a live affinity binding and the bound credential is ready, that
// credential is reused. Otherwise the configured selector picks one; if the
// session had no prior binding, the freshly-picked credential is bound.
func (d *DefaultConductor) Acquire(ctx context.Context, model, sessionID string) (*Credential, error) {
	hadBinding := false
	if d.runtime != nil && sessionID != "" {
		if id, ok, err := d.runtime.GetAffinity(ctx, sessionID, affinityTTL(d.affinity)); err == nil && ok {
			hadBinding = true
			if c := d.sched.Lookup(id); c != nil {
				if reserved, ok, err := d.tryReserve(ctx, c, model); err != nil {
					return nil, err
				} else if ok {
					d.trackReservation(ctx, c, reserved)
					d.maybeRefresh(ctx, c)
					return c, nil
				}
			}
		} else if err != nil {
			slog.WarnContext(ctx, "pool: redis affinity lookup failed", "err", err)
		}
	}
	if d.affinity != nil && sessionID != "" {
		if id, ok := d.affinity.Get(sessionID); ok {
			hadBinding = true
			if c := d.sched.Lookup(id); c != nil {
				if d.runtime != nil {
					if reserved, ok, err := d.tryReserve(ctx, c, model); err != nil {
						return nil, err
					} else if ok {
						d.trackReservation(ctx, c, reserved)
						d.maybeRefresh(ctx, c)
						return c, nil
					}
				} else if d.localReserve(c, model) {
					d.maybeRefresh(ctx, c)
					return c, nil
				}
			}
			// Bound cred is gone or in cooldown: fall through to selector,
			// but do NOT rewrite affinity (sticky-on-recovery semantics).
		}
	}

	c, err := d.pickAndReserve(ctx, model)
	if err != nil {
		return nil, err
	}

	if d.affinity != nil && sessionID != "" && !hadBinding {
		d.affinity.Set(sessionID, c.ID)
	}
	if d.runtime != nil && sessionID != "" && !hadBinding {
		if err := d.runtime.SetAffinity(ctx, sessionID, c.ID, affinityTTL(d.affinity)); err != nil {
			slog.WarnContext(ctx, "pool: redis affinity set failed", "err", err)
		}
	}
	d.maybeRefresh(ctx, c)
	return c, nil
}

func (d *DefaultConductor) pickAndReserve(ctx context.Context, model string) (*Credential, error) {
	for {
		var ready []*Credential
		if d.runtime != nil {
			all := d.sched.All()
			if err := d.runtime.SyncInFlight(ctx, all, model); err != nil {
				slog.WarnContext(ctx, "pool: redis in-flight sync failed", "err", err)
			}
			ready = d.sched.Ready()
		} else {
			ready = d.sched.Ready()
		}
		ready = filterReadyForModel(ready, model)
		if hint := RegionHintFrom(ctx); hint != "" {
			ready = FilterByRegion(ready, hint)
		}
		c, err := d.selector.Pick(ready, model)
		if err != nil {
			return nil, err
		}
		if d.runtime != nil {
			reserved, ok, err := d.tryReserve(ctx, c, model)
			if err != nil {
				return nil, err
			}
			if ok {
				d.trackReservation(ctx, c, reserved)
				return c, nil
			}
		} else if d.localReserve(c, model) {
			return c, nil
		}
		// A concurrent request can fill the account's MaxInFlight slot between
		// Ready/Pick and Reserve. Retry with a fresh snapshot; if that was the
		// last selectable credential, the next Pick returns ErrNoReady.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
}

func (d *DefaultConductor) tryReserve(ctx context.Context, c *Credential, model string) (RuntimeReservation, bool, error) {
	if d.runtime == nil {
		return RuntimeReservation{}, false, nil
	}
	res, ok, err := d.runtime.TryReserve(ctx, c, model, d.leaseTTL)
	if err != nil || !ok {
		return res, ok, err
	}
	if !d.localReserveRuntimeAccepted(c, model) {
		if releaseErr := d.runtime.Release(ctx, res); releaseErr != nil {
			slog.WarnContext(ctx, "pool: redis reservation rollback failed",
				"cred_id", c.ID, "reservation_id", res.ID, "err", releaseErr)
		}
		return RuntimeReservation{}, false, nil
	}
	return res, true, nil
}

func (d *DefaultConductor) trackReservation(ctx context.Context, c *Credential, res RuntimeReservation) {
	if d.runtime == nil || d.reservations == nil {
		return
	}
	renewCtx, cancel := context.WithCancel(ctx)
	d.reservations.push(c, res, cancel)
	go d.renewReservation(renewCtx, res)
}

func (d *DefaultConductor) renewReservation(ctx context.Context, res RuntimeReservation) {
	interval := reservationRenewInterval(d.leaseTTL)
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			renewCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			ok, err := d.runtime.Extend(renewCtx, res, d.leaseTTL)
			cancel()
			if err != nil {
				slog.Warn("pool: redis reservation renew failed",
					"cred_id", res.CredID, "reservation_id", res.ID, "err", err)
				timer.Reset(reservationRenewRetryInterval(interval))
				continue
			}
			if !ok {
				return
			}
			timer.Reset(interval)
		}
	}
}

func reservationRenewInterval(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return time.Minute
	}
	interval := ttl / 3
	if interval < 100*time.Millisecond {
		interval = 100 * time.Millisecond
	}
	if interval > time.Minute {
		interval = time.Minute
	}
	return interval
}

func reservationRenewRetryInterval(base time.Duration) time.Duration {
	if base <= 0 {
		return time.Second
	}
	retry := base / 4
	if retry < time.Second {
		return time.Second
	}
	if retry > 15*time.Second {
		return 15 * time.Second
	}
	return retry
}

func (d *DefaultConductor) localReserve(c *Credential, model string) bool {
	return c.Reserve(model)
}

func (d *DefaultConductor) localReserveRuntimeAccepted(c *Credential, model string) bool {
	c.Mu.Lock()
	defer c.Mu.Unlock()
	if c.Disabled {
		return false
	}
	c.InFlight++
	if model != "" {
		if c.InFlightByModel == nil {
			c.InFlightByModel = make(map[string]int64)
		}
		c.InFlightByModel[model]++
	}
	return true
}

func affinityTTL(a *Affinity) time.Duration {
	if a == nil || a.ttl <= 0 {
		return 30 * time.Minute
	}
	return a.ttl
}

func filterReadyForModel(creds []*Credential, model string) []*Credential {
	if len(creds) == 0 {
		return creds
	}
	out := creds[:0]
	for _, c := range creds {
		if c.IsReadyFor(model) {
			out = append(out, c)
		}
	}
	return out
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

// Release marks the credential as recently used and releases one in-flight
// slot reserved by Acquire.
func (d *DefaultConductor) Release(cred *Credential, model ...string) {
	if cred == nil {
		return
	}
	m := ""
	if len(model) > 0 {
		m = model[0]
	}
	if d.runtime != nil && d.reservations != nil {
		if res, cancel, ok := d.reservations.pop(cred, m); ok {
			if cancel != nil {
				cancel()
			}
			if err := d.runtime.Release(context.Background(), res); err != nil {
				slog.Warn("pool: redis reservation release failed", "cred_id", cred.ID, "err", err)
			}
		}
	}
	if releaser, ok := d.sched.(interface {
		ReleaseReservation(cred *Credential, model string) bool
	}); ok && releaser.ReleaseReservation(cred, m) {
		return
	}
	cred.ReleaseReservation(m)
}
