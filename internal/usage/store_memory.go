package usage

import (
	"context"
	"sync"
	"time"
)

// defaultMemoryCapacity is the default ring-buffer size used when callers
// pass a non-positive capacity to NewMemoryStore.
const defaultMemoryCapacity = 10000

// MemoryStore is a thread-safe ring-buffer Store implementation. When the
// buffer is full, Append overwrites the oldest record.
type MemoryStore struct {
	mu   sync.RWMutex
	buf  []Record
	head int  // index of the next write position
	full bool // true once buf has been filled at least once
	cap  int
}

// NewMemoryStore returns a MemoryStore with the given capacity. A
// non-positive capacity falls back to defaultMemoryCapacity.
func NewMemoryStore(capacity int) *MemoryStore {
	if capacity <= 0 {
		capacity = defaultMemoryCapacity
	}
	return &MemoryStore{
		buf: make([]Record, 0, capacity),
		cap: capacity,
	}
}

// Append adds rec to the ring buffer, overwriting the oldest entry once
// the buffer is full. Always returns nil.
func (m *MemoryStore) Append(rec Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.buf) < m.cap {
		m.buf = append(m.buf, rec)
		return nil
	}
	m.buf[m.head] = rec
	m.head = (m.head + 1) % m.cap
	m.full = true
	return nil
}

// Len returns the number of records currently stored.
func (m *MemoryStore) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.buf)
}

// Oldest returns the timestamp of the oldest record in the buffer, or
// the zero value if the buffer is empty.
func (m *MemoryStore) Oldest() time.Time {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.buf) == 0 {
		return time.Time{}
	}
	if m.full {
		return m.buf[m.head].Timestamp
	}
	return m.buf[0].Timestamp
}

// Query rolls up records matching filter within window.
func (m *MemoryStore) Query(ctx context.Context, filter Filter, window Window) (Aggregate, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	agg := Aggregate{
		ByCredModel: make(map[string]map[string]CellStats),
	}

	var timeline []TimelineBucket
	var bucketNanos int64
	if window.Bucket > 0 && !window.End.Before(window.Start) {
		bucketNanos = window.Bucket.Nanoseconds()
		duration := window.End.Sub(window.Start)
		n := int((duration + window.Bucket - 1) / window.Bucket) // ceil
		if n < 0 {
			n = 0
		}
		timeline = make([]TimelineBucket, n)
		for i := 0; i < n; i++ {
			timeline[i].Start = window.Start.Add(time.Duration(i) * window.Bucket)
		}
	}

	for _, rec := range m.buf {
		if rec.Timestamp.Before(window.Start) || !rec.Timestamp.Before(window.End) {
			continue
		}
		if !matchFilter(rec, filter) {
			continue
		}
		applyToAggregate(&agg, rec)

		if bucketNanos > 0 {
			offset := rec.Timestamp.Sub(window.Start).Nanoseconds()
			idx := int(offset / bucketNanos)
			if idx >= 0 && idx < len(timeline) {
				b := &timeline[idx]
				b.Requests++
				if rec.Status == StatusSuccess {
					b.Success++
				} else {
					b.Failed++
				}
				b.InputTokens += int64(rec.InputTokens)
				b.OutputTokens += int64(rec.OutputTokens)
			}
		}
	}

	agg.Timeline = timeline
	return agg, nil
}

// Recent returns the most recent records matching filter, sorted by
// Timestamp descending. limit must be positive; values exceeding the
// buffer's content are clamped.
func (m *MemoryStore) Recent(_ context.Context, filter Filter, limit int) ([]Record, error) {
	if limit <= 0 {
		return nil, nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	// Walk the ring from newest to oldest.
	cap := limit
	if cap > len(m.buf) {
		cap = len(m.buf)
	}
	out := make([]Record, 0, cap)
	n := len(m.buf)
	for i := 0; i < n; i++ {
		var idx int
		if m.full {
			// head points at the oldest; newest is head-1.
			idx = (m.head - 1 - i + m.cap) % m.cap
		} else {
			idx = n - 1 - i
		}
		rec := m.buf[idx]
		if !matchFilter(rec, filter) {
			continue
		}
		out = append(out, rec)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// Close is a no-op for MemoryStore.
func (m *MemoryStore) Close() error { return nil }

// matchFilter reports whether rec passes filter. An empty list within a
// field matches everything for that field.
func matchFilter(rec Record, f Filter) bool {
	if len(f.CredentialIDs) > 0 && !containsString(f.CredentialIDs, rec.CredentialID) {
		return false
	}
	if len(f.Models) > 0 && !containsString(f.Models, rec.ResolvedModel) {
		return false
	}
	if len(f.Statuses) > 0 && !containsString(f.Statuses, rec.Status) {
		return false
	}
	if len(f.APIKeyIDs) > 0 && !containsString(f.APIKeyIDs, rec.APIKeyID) {
		return false
	}
	if len(f.DeviceIDs) > 0 && !containsString(f.DeviceIDs, rec.DeviceID) {
		return false
	}
	return true
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// applyToAggregate folds rec into the totals and the cell map.
func applyToAggregate(agg *Aggregate, rec Record) {
	agg.TotalRequests++
	if rec.Status == StatusSuccess {
		agg.TotalSuccess++
	} else {
		agg.TotalFailed++
	}
	agg.TotalInputTokens += int64(rec.InputTokens)
	agg.TotalOutputTokens += int64(rec.OutputTokens)
	agg.TotalCacheRead += int64(rec.CacheReadTokens)
	agg.TotalCacheWrite += int64(rec.CacheWriteTokens)

	inner, ok := agg.ByCredModel[rec.CredentialID]
	if !ok {
		inner = make(map[string]CellStats)
		agg.ByCredModel[rec.CredentialID] = inner
	}
	cell := inner[rec.ResolvedModel]
	addToCell(&cell, rec)
	inner[rec.ResolvedModel] = cell

	if agg.ByAPIKey == nil {
		agg.ByAPIKey = make(map[string]CellStats)
	}
	keyCell := agg.ByAPIKey[rec.APIKeyID]
	addToCell(&keyCell, rec)
	agg.ByAPIKey[rec.APIKeyID] = keyCell

	if agg.ByDevice == nil {
		agg.ByDevice = make(map[string]CellStats)
	}
	devCell := agg.ByDevice[rec.DeviceID]
	addToCell(&devCell, rec)
	agg.ByDevice[rec.DeviceID] = devCell
}

func addToCell(cell *CellStats, rec Record) {
	cell.Requests++
	if rec.Status == StatusSuccess {
		cell.Success++
	} else {
		cell.Failed++
	}
	cell.InputTokens += int64(rec.InputTokens)
	cell.OutputTokens += int64(rec.OutputTokens)
	cell.CacheReadTokens += int64(rec.CacheReadTokens)
	cell.CacheWriteTokens += int64(rec.CacheWriteTokens)
	if rec.Timestamp.After(cell.LastSeenAt) {
		cell.LastSeenAt = rec.Timestamp
	}
}
