// Package quota fetches Kiro's getUsageLimits endpoint and writes snapshots
// back to the pool. A Poller drives periodic refresh in the background; the
// Fetcher can also be invoked on-demand from the admin API.
//
// Endpoint reference: cockpit-tools/crates/cockpit-core/src/modules/kiro_oauth.rs
//
//	GET https://q.{region}.amazonaws.com/getUsageLimits
//	    ?origin=AI_EDITOR
//	    &profileArn={url_encoded_arn}
//	    &resourceType=AGENTIC_REQUEST
//	    [&isEmailRequired=true]
//	Headers: Authorization: Bearer {access_token}
package quota

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/niuma/kirocc-pro/internal/pool"
)

// DefaultPollInterval is the wall-clock period between automatic refreshes.
const DefaultPollInterval = 3 * time.Minute

// DefaultCacheTTL is how long a cached snapshot is considered fresh.
const DefaultCacheTTL = 60 * time.Second

// DefaultMaxConcurrent is the upper bound on parallel quota fetches.
const DefaultMaxConcurrent = 5

// KiroEndpoint returns the upstream URL for a given region. Empty or
// unrecognized regions fall back to us-east-1.
func KiroEndpoint(region string) string {
	r := strings.ToLower(strings.TrimSpace(region))
	switch r {
	case "us-gov-east-1":
		return "https://q-fips.us-gov-east-1.amazonaws.com"
	case "us-gov-west-1":
		return "https://q-fips.us-gov-west-1.amazonaws.com"
	case "":
		r = "us-east-1"
	}
	return fmt.Sprintf("https://q.%s.amazonaws.com", r)
}

// Fetcher knows how to query the Kiro getUsageLimits endpoint.
type Fetcher interface {
	Fetch(ctx context.Context, accessToken, profileARN, region string) (*pool.KiroQuotaSnapshot, error)
}

// Cache is a TTL-cached wrapper over a Fetcher. Fetch returns a cached
// snapshot if it is younger than the cache's TTL; FetchForce bypasses the
// cache. Cache is safe for concurrent use.
//
// Implementation provided in cache_impl.go.
type Cache interface {
	Fetch(ctx context.Context, credID, accessToken, profileARN, region string) (*pool.KiroQuotaSnapshot, error)
	FetchForce(ctx context.Context, credID, accessToken, profileARN, region string) (*pool.KiroQuotaSnapshot, error)
}

// Poller drives periodic quota refresh for the pool. Each tick iterates
// the scheduler's non-disabled credentials and fetches each one (semaphore-
// limited). Successful results are written back via scheduler.RefreshQuota;
// failures are recorded via scheduler.RecordQuotaError.
//
// Implementation provided in poller_impl.go.
type Poller interface {
	// Run blocks until ctx is cancelled. Enabled pollers perform one immediate
	// refresh, then tick every interval. A zero-interval poller is disabled and
	// only waits for ctx cancellation.
	Run(ctx context.Context)
}
