// Package usage tracks per-request token accounting. It pairs each upstream
// call with the credential and resolved model so usage can be aggregated by
// credential, model, or time bucket.
//
// The default backend pairs an in-memory ring (for fast recent queries)
// with a SQLite append log (for arbitrary-window queries). Both share the
// same Record schema.
package usage

import (
	"context"
	"time"
)

// Status values reported alongside each Record.
const (
	StatusSuccess       = "success"
	StatusRateLimited   = "rate_limited"
	StatusAuthError     = "auth_error"
	StatusUpstreamError = "upstream_error"
)

// Record is one observation of an upstream call.
type Record struct {
	Timestamp        time.Time
	CredentialID     string // pool.Credential.ID; "" if pool disabled (legacy single-cred mode)
	Provider         string // "kiro" (v1)
	RequestedModel   string // client-supplied, e.g. "claude-opus-4-7"
	ResolvedModel    string // upstream SKU, e.g. "claude-opus-4.7"
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	Status           string
	LatencyMs        int
	TraceID          string

	// [fork] Capture fields for the admin history panel.
	Type                 string  // "stream" | "non-stream"
	Device               string  // short User-Agent excerpt, e.g. "claude-code/1.0.0"
	DeviceID             string  // stable hash(ip+ua) for grouping; "" if no fingerprint
	APIKeyID             string  // matched dynamic API key id; "" for legacy single-key auth
	CreditsUsedSnapshot  float64 // cred.LastQuota.CreditsUsed at publish time; 0 = unknown
	CreditsTotalSnapshot float64 // cred.LastQuota.CreditsTotal at publish time; 0 = unknown
}

// Aggregator is the high-level facade combining a Store with in-memory
// indexes for fast recent-window queries. Implementations must be safe
// for concurrent use.
type Aggregator interface {
	// Publish records r. Non-blocking; errors are logged not returned.
	Publish(r Record)

	// Query returns rolled-up stats matching filter within window.
	Query(ctx context.Context, filter Filter, window Window) (Aggregate, error)

	// Recent returns the most recent records matching filter, sorted by
	// Timestamp descending. limit must be positive; values above the
	// implementation's cap are clamped silently.
	Recent(ctx context.Context, filter Filter, limit int) ([]Record, error)

	// Close releases resources (flushes pending writes, closes DB handle).
	Close() error
}

// Filter narrows a query.
type Filter struct {
	CredentialIDs []string // OR within the field
	Models        []string // matches ResolvedModel
	Statuses      []string
	APIKeyIDs     []string // matches Record.APIKeyID
	DeviceIDs     []string // matches Record.DeviceID
}

// Window selects a time range and optional bucket size for timelines.
// If Bucket is zero, Timeline is empty in the result.
type Window struct {
	Start, End time.Time
	Bucket     time.Duration
}

// Aggregate is the rollup of a query.
type Aggregate struct {
	TotalRequests     int64
	TotalSuccess      int64
	TotalFailed       int64
	TotalInputTokens  int64
	TotalOutputTokens int64
	TotalCacheRead    int64
	TotalCacheWrite   int64

	// ByCredModel[credID][resolvedModel] is the per-cell rollup.
	ByCredModel map[string]map[string]CellStats

	// ByAPIKey[api_key_id] rolls up usage attributed to each dynamic API
	// key (matched via the /v1/messages Authorization: Bearer header).
	// Records authenticated via the legacy single -api-key flag fall under
	// the empty-string key.
	ByAPIKey map[string]CellStats

	// ByDevice[device_id] rolls up usage by the sha256(IP+UA) fingerprint
	// the proxy assigns at auth time.
	ByDevice map[string]CellStats

	// Timeline contains one bucket per Window.Bucket between Start and End.
	// Empty if Window.Bucket == 0.
	Timeline []TimelineBucket
}

// CellStats is one row of ByCredModel.
type CellStats struct {
	Requests          int64
	Success           int64
	Failed            int64
	InputTokens       int64
	OutputTokens      int64
	CacheReadTokens   int64
	CacheWriteTokens  int64
	LastSeenAt        time.Time
}

// TimelineBucket is one time-bucket row in Aggregate.Timeline.
type TimelineBucket struct {
	Start        time.Time
	Requests     int64
	Success      int64
	Failed       int64
	InputTokens  int64
	OutputTokens int64
}

// Store is the persistence layer used by the default Aggregator. Two
// implementations are provided: memory (ring buffer) and sqlite (append log).
type Store interface {
	Append(rec Record) error
	Query(ctx context.Context, filter Filter, window Window) (Aggregate, error)
	Recent(ctx context.Context, filter Filter, limit int) ([]Record, error)
	Close() error
}
