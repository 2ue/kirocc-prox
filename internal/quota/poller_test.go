package quota

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/niuma/kirocc-pro/internal/auth"
	"github.com/niuma/kirocc-pro/internal/pool"
)

// stubScheduler is the minimal pool.Scheduler used by poller tests. Only the
// methods the poller actually calls (All / RefreshQuota / RecordQuotaError /
// MarkAuthError) record activity; the rest are no-ops.
type stubScheduler struct {
	mu    sync.Mutex
	creds []*pool.Credential

	refreshCalls       map[string]*pool.KiroQuotaSnapshot
	recordErrorCalls   map[string]string
	markAuthErrorCalls map[string]string
}

func newStubScheduler(creds ...*pool.Credential) *stubScheduler {
	return &stubScheduler{
		creds:              creds,
		refreshCalls:       make(map[string]*pool.KiroQuotaSnapshot),
		recordErrorCalls:   make(map[string]string),
		markAuthErrorCalls: make(map[string]string),
	}
}

func (s *stubScheduler) Register(creds []*pool.Credential) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.creds = creds
}

func (s *stubScheduler) Ready() []*pool.Credential { return s.All() }

func (s *stubScheduler) Lookup(id string) *pool.Credential {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.creds {
		if c.ID == id {
			return c
		}
	}
	return nil
}

func (s *stubScheduler) All() []*pool.Credential {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*pool.Credential, len(s.creds))
	copy(out, s.creds)
	return out
}

func (s *stubScheduler) MarkSuccess(_, _ string, _ pool.Usage)             {}
func (s *stubScheduler) MarkRateLimit(_, _ string, _ time.Duration)        {}

func (s *stubScheduler) MarkAuthError(credID, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.markAuthErrorCalls[credID] = reason
}

func (s *stubScheduler) RefreshQuota(credID string, snap *pool.KiroQuotaSnapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshCalls[credID] = snap
}

func (s *stubScheduler) RecordQuotaError(credID, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordErrorCalls[credID] = errMsg
}

func (s *stubScheduler) SetEnabled(_ string, _ bool) error { return nil }
func (s *stubScheduler) Add(_ *pool.Credential) error      { return nil }
func (s *stubScheduler) Remove(_ string) error             { return nil }

// Snapshot accessors for tests.
func (s *stubScheduler) refreshCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.refreshCalls)
}

func (s *stubScheduler) errorCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.recordErrorCalls)
}

// programmableFetcher returns canned snapshots / errors keyed by access token.
type programmableFetcher struct {
	mu     sync.Mutex
	byTok  map[string]*pool.KiroQuotaSnapshot
	errs   map[string]error
	calls  atomic.Int64
}

func newProgrammableFetcher() *programmableFetcher {
	return &programmableFetcher{
		byTok: make(map[string]*pool.KiroQuotaSnapshot),
		errs:  make(map[string]error),
	}
}

func (f *programmableFetcher) setSnap(token string, snap *pool.KiroQuotaSnapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byTok[token] = snap
}

func (f *programmableFetcher) setErr(token string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs[token] = err
}

func (f *programmableFetcher) Fetch(_ context.Context, token, _, _ string) (*pool.KiroQuotaSnapshot, error) {
	f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.errs[token]; ok {
		return nil, err
	}
	if snap, ok := f.byTok[token]; ok {
		c := *snap
		return &c, nil
	}
	return &pool.KiroQuotaSnapshot{FetchedAt: time.Now(), PlanName: "default"}, nil
}

func makeCred(id, token string, disabled bool) *pool.Credential {
	c := &pool.Credential{
		ID:       id,
		Label:    id,
		Disabled: disabled,
	}
	c.AccessToken = token
	c.ProfileARN = "arn-" + id
	c.Region = "us-east-1"
	return c
}

// makeCredWithAuth fills the embedded auth.Credentials struct directly to
// exercise the lock path used by the poller.
func makeCredWithAuth(id string, ac auth.Credentials) *pool.Credential {
	return &pool.Credential{
		ID:          id,
		Label:       id,
		Credentials: ac,
	}
}

func TestPoller_TickRefreshesAllNonDisabled(t *testing.T) {
	good := makeCred("good-1", "tok-good", false)
	disabled := makeCred("disabled-1", "tok-disabled", true)
	bad := makeCred("bad-1", "tok-bad", false)

	sched := newStubScheduler(good, disabled, bad)

	fetcher := newProgrammableFetcher()
	fetcher.setSnap("tok-good", &pool.KiroQuotaSnapshot{PlanName: "Pro", FetchedAt: time.Now()})
	fetcher.setErr("tok-bad", errors.New("upstream 500"))

	cache := NewCache(fetcher, time.Millisecond) // tiny TTL — FetchForce bypasses anyway
	p := NewPoller(sched, cache, time.Hour, 4)   // long interval; only the immediate refresh fires

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(done)
	}()

	// Wait for the immediate refresh to settle.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sched.refreshCount() == 1 && sched.errorCount() == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}

	if got := sched.refreshCount(); got != 1 {
		t.Errorf("refresh calls = %d, want 1", got)
	}
	if got := sched.errorCount(); got != 1 {
		t.Errorf("record-error calls = %d, want 1", got)
	}
	sched.mu.Lock()
	if _, ok := sched.refreshCalls["good-1"]; !ok {
		t.Error("good-1 not refreshed")
	}
	if _, ok := sched.refreshCalls["disabled-1"]; ok {
		t.Error("disabled-1 should be skipped")
	}
	if msg, ok := sched.recordErrorCalls["bad-1"]; !ok || msg == "" {
		t.Errorf("bad-1 record-error = %q, want non-empty", msg)
	}
	sched.mu.Unlock()
}

func TestPoller_ContextCancelStopsRun(t *testing.T) {
	sched := newStubScheduler()
	cache := NewCache(newProgrammableFetcher(), time.Second)
	p := NewPoller(sched, cache, 10*time.Millisecond, 1)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

func TestPoller_DefaultsApplied(t *testing.T) {
	sched := newStubScheduler()
	p := NewPoller(sched, NewCache(newProgrammableFetcher(), time.Second), 0, 0)
	dp, ok := p.(*DefaultPoller)
	if !ok {
		t.Fatal("NewPoller did not return *DefaultPoller")
	}
	if dp.interval != DefaultPollInterval {
		t.Errorf("interval = %v, want %v", dp.interval, DefaultPollInterval)
	}
	if dp.maxConcurrent != DefaultMaxConcurrent {
		t.Errorf("maxConcurrent = %d, want %d", dp.maxConcurrent, DefaultMaxConcurrent)
	}
}

func TestPoller_UsesEmbeddedAuthCredentials(t *testing.T) {
	// Sanity check that the poller reads the embedded auth.Credentials
	// fields (AccessToken / ProfileARN / Region), not zero values.
	cred := makeCredWithAuth("auth-1", auth.Credentials{
		AccessToken: "tok-auth",
		ProfileARN:  "arn-auth",
		Region:      "eu-west-1",
	})
	sched := newStubScheduler(cred)

	fetcher := newProgrammableFetcher()
	fetcher.setSnap("tok-auth", &pool.KiroQuotaSnapshot{PlanName: "AuthPlan"})

	cache := NewCache(fetcher, time.Millisecond)
	p := NewPoller(sched, cache, time.Hour, 1)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sched.refreshCount() == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	sched.mu.Lock()
	snap, ok := sched.refreshCalls["auth-1"]
	sched.mu.Unlock()
	if !ok || snap == nil {
		t.Fatal("auth-1 not refreshed")
	}
	if snap.PlanName != "AuthPlan" {
		t.Errorf("PlanName = %q, want AuthPlan (fetcher dispatch by token failed)", snap.PlanName)
	}
}
