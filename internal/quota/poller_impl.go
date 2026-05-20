package quota

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/niuma/kirocc-pro/internal/pool"
)

// DefaultPoller drives periodic refresh of all non-disabled credentials.
type DefaultPoller struct {
	scheduler     pool.Scheduler
	cache         Cache
	interval      time.Duration
	maxConcurrent int
}

// NewPoller constructs a poller. Non-positive values fall back to the
// package defaults.
func NewPoller(s pool.Scheduler, c Cache, interval time.Duration, maxConcurrent int) Poller {
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	if maxConcurrent <= 0 {
		maxConcurrent = DefaultMaxConcurrent
	}
	return &DefaultPoller{
		scheduler:     s,
		cache:         c,
		interval:      interval,
		maxConcurrent: maxConcurrent,
	}
}

// Run blocks until ctx is cancelled. One immediate refresh runs before the
// first tick.
func (p *DefaultPoller) Run(ctx context.Context) {
	p.refreshAll(ctx)

	t := time.NewTicker(p.interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.refreshAll(ctx)
		}
	}
}

// refreshAll iterates the scheduler's credentials and fans out fetches up to
// maxConcurrent in parallel. Returns once every fetch has settled (or ctx is
// cancelled).
func (p *DefaultPoller) refreshAll(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	creds := p.scheduler.All()
	if len(creds) == 0 {
		return
	}

	sem := make(chan struct{}, p.maxConcurrent)
	var wg sync.WaitGroup

	for _, cred := range creds {
		// Skip disabled credentials — Snapshot acquires the read lock.
		view := cred.Snapshot()
		if view.Disabled {
			continue
		}

		// Snapshot the fields we need under the credential lock to avoid
		// racing with auth refresh.
		cred.Mu.RLock()
		credID := cred.ID
		token := cred.AccessToken
		arn := cred.ProfileARN
		region := cred.Region
		cred.Mu.RUnlock()

		// Skip credentials that have not been initialized yet (single-account
		// mode before the first request loads the SQLite DB, or a JSON entry
		// missing required fields). Polling would just produce HTTP 400s.
		if token == "" || arn == "" {
			slog.DebugContext(ctx, "quota: skipping credential without token/profileArn",
				"cred_id", credID)
			continue
		}

		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case sem <- struct{}{}:
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			p.refreshOne(ctx, credID, token, arn, region)
		}()
	}

	wg.Wait()
}

func (p *DefaultPoller) refreshOne(ctx context.Context, credID, token, arn, region string) {
	snap, err := p.cache.FetchForce(ctx, credID, token, arn, region)
	if err != nil {
		slog.WarnContext(ctx, "quota: refresh failed",
			"cred_id", credID, "err", err)
		p.scheduler.RecordQuotaError(credID, err.Error())
		return
	}
	p.scheduler.RefreshQuota(credID, snap)
}
