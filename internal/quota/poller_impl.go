package quota

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
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

	credentialFetcher CredentialFetcher
	refresher         TokenRefresher

	backoffMu sync.Mutex
	backoff   map[string]pollBackoff
}

type disabledPoller struct{}

// CredentialFetcher fetches quota from a live credential instead of a token
// snapshot. Provider registries implement this to honor per-account routing.
type CredentialFetcher interface {
	FetchQuota(ctx context.Context, cred *pool.Credential) (*pool.KiroQuotaSnapshot, error)
}

// TokenRefresher refreshes one credential in place.
type TokenRefresher interface {
	Refresh(ctx context.Context, cred *pool.Credential) error
}

type PollerOption func(*DefaultPoller)

func WithCredentialFetcher(fetcher CredentialFetcher) PollerOption {
	return func(p *DefaultPoller) {
		p.credentialFetcher = fetcher
	}
}

func WithRefresher(refresher TokenRefresher) PollerOption {
	return func(p *DefaultPoller) {
		p.refresher = refresher
	}
}

type pollBackoff struct {
	failures int
	until    time.Time
}

// NewPoller constructs a poller. An interval of 0 disables polling; negative
// intervals fall back to the package default for direct package callers.
func NewPoller(s pool.Scheduler, c Cache, interval time.Duration, maxConcurrent int, opts ...PollerOption) Poller {
	if interval == 0 {
		return disabledPoller{}
	}
	if interval < 0 {
		interval = DefaultPollInterval
	}
	if maxConcurrent <= 0 {
		maxConcurrent = DefaultMaxConcurrent
	}
	p := &DefaultPoller{
		scheduler:     s,
		cache:         c,
		interval:      interval,
		maxConcurrent: maxConcurrent,
		backoff:       make(map[string]pollBackoff),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(p)
		}
	}
	return p
}

func (disabledPoller) Run(ctx context.Context) {
	<-ctx.Done()
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

		// Skip credentials that have not been initialized yet. Polling would
		// just produce HTTP 400s.
		if token == "" || arn == "" {
			slog.DebugContext(ctx, "quota: skipping credential without token/profileArn",
				"cred_id", credID)
			continue
		}
		if p.shouldSkipBackoff(credID, time.Now()) {
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
			p.refreshOne(ctx, cred, credID, token, arn, region)
		}()
	}

	wg.Wait()
}

func (p *DefaultPoller) refreshOne(ctx context.Context, cred *pool.Credential, credID, token, arn, region string) {
	snap, err := p.fetchQuota(ctx, cred, credID, token, arn, region)
	if err != nil {
		if isQuotaAuthError(err) && p.refresher != nil && cred != nil {
			refreshErr := p.refresher.Refresh(ctx, cred)
			if refreshErr == nil {
				token, arn, region = readQuotaInputs(cred)
				snap, err = p.fetchQuota(ctx, cred, credID, token, arn, region)
				if err == nil {
					p.scheduler.RefreshQuota(credID, snap)
					p.clearBackoff(credID)
					return
				}
			} else {
				err = fmt.Errorf("token refresh: %w (original: %v)", refreshErr, err)
			}
		}
		slog.WarnContext(ctx, "quota: refresh failed",
			"cred_id", credID, "err", err)
		p.scheduler.RecordQuotaError(credID, err.Error())
		p.recordFailure(credID, time.Now())
		return
	}
	p.scheduler.RefreshQuota(credID, snap)
	p.clearBackoff(credID)
}

func (p *DefaultPoller) fetchQuota(ctx context.Context, cred *pool.Credential, credID, token, arn, region string) (*pool.KiroQuotaSnapshot, error) {
	if p.credentialFetcher != nil && cred != nil {
		return p.credentialFetcher.FetchQuota(ctx, cred)
	}
	return p.cache.FetchForce(ctx, credID, token, arn, region)
}

func readQuotaInputs(cred *pool.Credential) (token, arn, region string) {
	if cred == nil {
		return "", "", ""
	}
	cred.Mu.RLock()
	defer cred.Mu.RUnlock()
	return cred.AccessToken, cred.ProfileARN, cred.Region
}

func (p *DefaultPoller) shouldSkipBackoff(credID string, now time.Time) bool {
	p.backoffMu.Lock()
	defer p.backoffMu.Unlock()
	b, ok := p.backoff[credID]
	if !ok {
		return false
	}
	if now.Before(b.until) {
		return true
	}
	return false
}

func (p *DefaultPoller) recordFailure(credID string, now time.Time) {
	p.backoffMu.Lock()
	defer p.backoffMu.Unlock()
	b := p.backoff[credID]
	b.failures++
	b.until = now.Add(nextPollBackoff(p.interval, b.failures))
	p.backoff[credID] = b
}

func (p *DefaultPoller) clearBackoff(credID string) {
	p.backoffMu.Lock()
	defer p.backoffMu.Unlock()
	delete(p.backoff, credID)
}

func nextPollBackoff(interval time.Duration, failures int) time.Duration {
	if failures <= 0 {
		failures = 1
	}
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	multiplier := 1 << min(failures, 4)
	d := time.Duration(multiplier) * interval
	maxBackoff := 30 * time.Minute
	if d > maxBackoff {
		return maxBackoff
	}
	if d < time.Minute {
		return time.Minute
	}
	return d
}

func isQuotaAuthError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "status 401") ||
		strings.Contains(msg, "status 403") ||
		strings.Contains(msg, "bearer token") ||
		strings.Contains(msg, "expired") ||
		strings.Contains(msg, "unauthorized") ||
		strings.Contains(msg, "forbidden")
}
