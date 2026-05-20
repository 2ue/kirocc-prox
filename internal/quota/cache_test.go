package quota

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/niuma/kirocc-pro/internal/pool"
)

// countingFetcher records every Fetch call and returns a canned snapshot.
type countingFetcher struct {
	calls atomic.Int64
	snap  *pool.KiroQuotaSnapshot
	err   error

	mu       sync.Mutex
	lastArgs []string
}

func (f *countingFetcher) Fetch(_ context.Context, accessToken, profileARN, region string) (*pool.KiroQuotaSnapshot, error) {
	f.calls.Add(1)
	f.mu.Lock()
	f.lastArgs = []string{accessToken, profileARN, region}
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	if f.snap == nil {
		return &pool.KiroQuotaSnapshot{FetchedAt: time.Now(), PlanName: "stub"}, nil
	}
	c := *f.snap
	return &c, nil
}

func TestCache_HitReducesCalls(t *testing.T) {
	f := &countingFetcher{}
	c := NewCache(f, time.Hour)

	if _, err := c.Fetch(context.Background(), "id", "t", "a", "us-east-1"); err != nil {
		t.Fatalf("Fetch 1 error: %v", err)
	}
	if _, err := c.Fetch(context.Background(), "id", "t", "a", "us-east-1"); err != nil {
		t.Fatalf("Fetch 2 error: %v", err)
	}
	if got := f.calls.Load(); got != 1 {
		t.Errorf("fetcher calls = %d, want 1", got)
	}
}

func TestCache_FetchForceAlwaysCalls(t *testing.T) {
	f := &countingFetcher{}
	c := NewCache(f, time.Hour)

	for i := 0; i < 3; i++ {
		if _, err := c.FetchForce(context.Background(), "id", "t", "a", "us-east-1"); err != nil {
			t.Fatalf("FetchForce %d error: %v", i, err)
		}
	}
	if got := f.calls.Load(); got != 3 {
		t.Errorf("fetcher calls = %d, want 3", got)
	}
}

func TestCache_TTLExpiryRefetches(t *testing.T) {
	f := &countingFetcher{}
	c := NewCache(f, 10*time.Millisecond)

	if _, err := c.Fetch(context.Background(), "id", "t", "a", "us-east-1"); err != nil {
		t.Fatalf("Fetch 1 error: %v", err)
	}
	time.Sleep(25 * time.Millisecond)
	if _, err := c.Fetch(context.Background(), "id", "t", "a", "us-east-1"); err != nil {
		t.Fatalf("Fetch 2 error: %v", err)
	}
	if got := f.calls.Load(); got != 2 {
		t.Errorf("fetcher calls = %d, want 2 (after TTL expiry)", got)
	}
}

func TestCache_PerCredIsolation(t *testing.T) {
	f := &countingFetcher{}
	c := NewCache(f, time.Hour)

	if _, err := c.Fetch(context.Background(), "a", "t", "arn", "us-east-1"); err != nil {
		t.Fatalf("Fetch a error: %v", err)
	}
	if _, err := c.Fetch(context.Background(), "b", "t", "arn", "us-east-1"); err != nil {
		t.Fatalf("Fetch b error: %v", err)
	}
	if got := f.calls.Load(); got != 2 {
		t.Errorf("fetcher calls = %d, want 2 (per-cred cache key)", got)
	}
}

func TestCache_ErrorDoesNotPopulate(t *testing.T) {
	f := &countingFetcher{err: errors.New("boom")}
	c := NewCache(f, time.Hour)

	if _, err := c.Fetch(context.Background(), "id", "t", "a", "us-east-1"); err == nil {
		t.Fatal("want error, got nil")
	}
	if _, err := c.Fetch(context.Background(), "id", "t", "a", "us-east-1"); err == nil {
		t.Fatal("want error on second call too")
	}
	if got := f.calls.Load(); got != 2 {
		t.Errorf("fetcher calls = %d, want 2 (errored fetches should not populate cache)", got)
	}
}

func TestCache_ConcurrentAccess(t *testing.T) {
	f := &countingFetcher{}
	c := NewCache(f, time.Hour)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.Fetch(context.Background(), "id", "t", "a", "us-east-1")
			_, _ = c.FetchForce(context.Background(), "id", "t", "a", "us-east-1")
		}()
	}
	wg.Wait()
}
