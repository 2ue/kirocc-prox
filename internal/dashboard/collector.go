package dashboard

import (
	"sync"
	"time"
)

// RequestRecord holds the metrics for a single completed request.
type RequestRecord struct {
	ID           string    `json:"id"`
	Time         time.Time `json:"time"`
	TraceID      string    `json:"trace_id"`
	SessionID    string    `json:"session_id"`
	Model        string    `json:"model"`
	Stream       bool      `json:"stream"`
	Thinking     bool      `json:"thinking"`
	InputTokens  int       `json:"input_tokens"`
	OutputTokens int       `json:"output_tokens"`
	ContextPct   float64   `json:"context_pct"`
	LatencyMs    int64     `json:"latency_ms"`
	RetryCount   int       `json:"retry_count"`
	Status       string    `json:"status"` // "ok" | "error" | "retry"
	ErrorMsg     string    `json:"error_msg,omitempty"`
}

// Stats holds aggregated metrics across all recorded requests.
type Stats struct {
	TotalRequests  int64   `json:"total_requests"`
	SuccessCount   int64   `json:"success_count"`
	ErrorCount     int64   `json:"error_count"`
	RetryCount     int64   `json:"retry_count"`
	SuccessRate    float64 `json:"success_rate"`
	RetryRate      float64 `json:"retry_rate"`
	AvgLatencyMs   float64 `json:"avg_latency_ms"`
	TotalInputTok  int64   `json:"total_input_tokens"`
	TotalOutputTok int64   `json:"total_output_tokens"`
}

// Collector is a thread-safe in-memory metrics store with SSE fan-out.
type Collector struct {
	mu      sync.RWMutex
	records []RequestRecord
	maxRecs int

	totalReqs      int64
	successCount   int64
	errorCount     int64
	retryCount     int64
	totalLatencyMs int64
	totalInputTok  int64
	totalOutputTok int64

	subsMu sync.Mutex
	subs   map[chan RequestRecord]struct{}
}

// NewCollector creates a Collector that retains at most maxRecords entries.
func NewCollector(maxRecords int) *Collector {
	if maxRecords <= 0 {
		maxRecords = 500
	}
	return &Collector{
		maxRecs: maxRecords,
		subs:    make(map[chan RequestRecord]struct{}),
	}
}

// Record stores a completed request and broadcasts it to SSE subscribers.
func (c *Collector) Record(r RequestRecord) {
	c.mu.Lock()
	if len(c.records) >= c.maxRecs {
		c.records = c.records[1:]
	}
	c.records = append(c.records, r)
	c.totalReqs++
	c.totalLatencyMs += r.LatencyMs
	c.totalInputTok += int64(r.InputTokens)
	c.totalOutputTok += int64(r.OutputTokens)
	switch r.Status {
	case "ok":
		c.successCount++
	case "error":
		c.errorCount++
	}
	if r.RetryCount > 0 {
		c.retryCount++
	}
	c.mu.Unlock()

	c.broadcast(r)
}

// Stats returns a snapshot of aggregated metrics.
func (c *Collector) Stats() Stats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	s := Stats{
		TotalRequests:  c.totalReqs,
		SuccessCount:   c.successCount,
		ErrorCount:     c.errorCount,
		RetryCount:     c.retryCount,
		TotalInputTok:  c.totalInputTok,
		TotalOutputTok: c.totalOutputTok,
	}
	if c.totalReqs > 0 {
		s.SuccessRate = float64(c.successCount) / float64(c.totalReqs) * 100
		s.RetryRate = float64(c.retryCount) / float64(c.totalReqs) * 100
		s.AvgLatencyMs = float64(c.totalLatencyMs) / float64(c.totalReqs)
	}
	return s
}

// RecentRecords returns the most recent n records (newest last).
func (c *Collector) RecentRecords(n int) []RequestRecord {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if n <= 0 || n > len(c.records) {
		n = len(c.records)
	}
	out := make([]RequestRecord, n)
	copy(out, c.records[len(c.records)-n:])
	return out
}

// Subscribe returns a channel that receives new RequestRecords as they arrive.
func (c *Collector) Subscribe() chan RequestRecord {
	ch := make(chan RequestRecord, 32)
	c.subsMu.Lock()
	c.subs[ch] = struct{}{}
	c.subsMu.Unlock()
	return ch
}

// Unsubscribe removes and closes a subscriber channel.
func (c *Collector) Unsubscribe(ch chan RequestRecord) {
	c.subsMu.Lock()
	delete(c.subs, ch)
	c.subsMu.Unlock()
	close(ch)
}

func (c *Collector) broadcast(r RequestRecord) {
	c.subsMu.Lock()
	defer c.subsMu.Unlock()
	for ch := range c.subs {
		select {
		case ch <- r:
		default:
			// Drop if subscriber is slow; never block the request path.
		}
	}
}
