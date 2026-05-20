package quota

import (
	"context"
	"sync"
	"time"

	"github.com/niuma/kirocc-pro/internal/pool"
)

// DefaultCache is the in-memory TTL cache implementation of Cache.
type DefaultCache struct {
	fetcher Fetcher
	ttl     time.Duration

	mu      sync.RWMutex
	entries map[string]cacheEntry
}

type cacheEntry struct {
	snap      *pool.KiroQuotaSnapshot
	fetchedAt time.Time
}

// NewCache constructs a TTL cache over the given fetcher.
func NewCache(f Fetcher, ttl time.Duration) Cache {
	if ttl <= 0 {
		ttl = DefaultCacheTTL
	}
	return &DefaultCache{
		fetcher: f,
		ttl:     ttl,
		entries: make(map[string]cacheEntry),
	}
}

// Fetch returns a cached snapshot if fresh, otherwise calls the underlying
// fetcher and stores the result.
func (c *DefaultCache) Fetch(ctx context.Context, credID, accessToken, profileARN, region string) (*pool.KiroQuotaSnapshot, error) {
	c.mu.RLock()
	e, ok := c.entries[credID]
	c.mu.RUnlock()
	if ok && time.Since(e.fetchedAt) < c.ttl {
		return e.snap, nil
	}
	return c.FetchForce(ctx, credID, accessToken, profileARN, region)
}

// FetchForce bypasses the cache, calls the fetcher, and updates the cache on
// success.
func (c *DefaultCache) FetchForce(ctx context.Context, credID, accessToken, profileARN, region string) (*pool.KiroQuotaSnapshot, error) {
	snap, err := c.fetcher.Fetch(ctx, accessToken, profileARN, region)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.entries[credID] = cacheEntry{snap: snap, fetchedAt: time.Now()}
	c.mu.Unlock()
	return snap, nil
}
