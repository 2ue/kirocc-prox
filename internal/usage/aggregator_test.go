package usage

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestAggregator_PublishFlowsToBothStores(t *testing.T) {
	t.Parallel()
	mem := NewMemoryStore(100)
	path := filepath.Join(t.TempDir(), "agg.sqlite")
	sqlStore, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	agg := NewAggregator(mem, sqlStore)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 25; i++ {
		agg.Publish(mkRecord(base.Add(time.Duration(i)*time.Second), "c1", "m", StatusSuccess, 1, 1))
	}

	// Close drains the worker so SQLite has all records.
	if err := agg.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := mem.Len(); got != 25 {
		t.Errorf("memory Len = %d, want 25", got)
	}

	// Re-open SQLite read-only to verify persistence.
	sqlStore2, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("reopen sqlite: %v", err)
	}
	defer func() { _ = sqlStore2.Close() }()
	result, err := sqlStore2.Query(context.Background(), Filter{}, Window{Start: base, End: base.Add(time.Hour)})
	if err != nil {
		t.Fatalf("query sqlite: %v", err)
	}
	if result.TotalRequests != 25 {
		t.Errorf("sqlite TotalRequests = %d, want 25", result.TotalRequests)
	}
}

func TestAggregator_QueryPrefersMemoryInRecentWindow(t *testing.T) {
	t.Parallel()
	mem := NewMemoryStore(100)
	path := filepath.Join(t.TempDir(), "agg.sqlite")
	sqlStore, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	agg := NewAggregator(mem, sqlStore)
	defer func() { _ = agg.Close() }()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		agg.Publish(mkRecord(base.Add(time.Duration(i)*time.Second), "c1", "m", StatusSuccess, 1, 1))
	}

	// Memory contains everything from base onward; querying a window that
	// starts at or after the oldest record should be served from memory.
	res, err := agg.Query(context.Background(), Filter{}, Window{Start: base, End: base.Add(time.Hour)})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if res.TotalRequests != 10 {
		t.Errorf("recent query TotalRequests = %d, want 10", res.TotalRequests)
	}
}

func TestAggregator_QueryFallsBackToSQLite(t *testing.T) {
	t.Parallel()
	// Small memory capacity forces the oldest records out of memory.
	mem := NewMemoryStore(3)
	path := filepath.Join(t.TempDir(), "agg.sqlite")
	sqlStore, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	agg := NewAggregator(mem, sqlStore)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// 10 records spread across 10 minutes. Memory keeps only the last 3
	// (i.e., t+7m..t+9m).
	for i := 0; i < 10; i++ {
		agg.Publish(mkRecord(base.Add(time.Duration(i)*time.Minute), "c", "m", StatusSuccess, 1, 1))
	}

	// Wait for the worker to drain pending sqlite writes.
	dagg := agg.(*DefaultAggregator)
	if !dagg.waitForDrain(2 * time.Second) {
		t.Fatalf("sqlite worker did not drain")
	}

	// Memory's oldest record is at base+7m; ask for a window starting at
	// base. Aggregator should fall back to SQLite which has all 10.
	res, err := agg.Query(context.Background(), Filter{}, Window{Start: base, End: base.Add(time.Hour)})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if res.TotalRequests != 10 {
		t.Errorf("fallback TotalRequests = %d, want 10", res.TotalRequests)
	}

	if err := agg.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestAggregator_CloseDrainsPendingWrites(t *testing.T) {
	t.Parallel()
	mem := NewMemoryStore(1000)
	path := filepath.Join(t.TempDir(), "drain.sqlite")
	sqlStore, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	agg := NewAggregator(mem, sqlStore)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 500; i++ {
		agg.Publish(mkRecord(base.Add(time.Duration(i)*time.Millisecond), "c", "m", StatusSuccess, 1, 1))
	}

	if err := agg.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Re-open and verify every record made it to disk.
	sqlStore2, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = sqlStore2.Close() }()
	res, err := sqlStore2.Query(context.Background(), Filter{}, Window{Start: base, End: base.Add(time.Hour)})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if res.TotalRequests != 500 {
		t.Errorf("after drain TotalRequests = %d, want 500", res.TotalRequests)
	}
}

func TestAggregator_MemoryOnly(t *testing.T) {
	t.Parallel()
	mem := NewMemoryStore(100)
	agg := NewAggregator(mem, nil)
	defer func() { _ = agg.Close() }()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		agg.Publish(mkRecord(base.Add(time.Duration(i)*time.Second), "c", "m", StatusSuccess, 1, 1))
	}
	res, err := agg.Query(context.Background(), Filter{}, Window{Start: base, End: base.Add(time.Hour)})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if res.TotalRequests != 5 {
		t.Errorf("mem-only TotalRequests = %d, want 5", res.TotalRequests)
	}
}

func TestAggregator_SQLiteOnly(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "sqlite-only.sqlite")
	sqlStore, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	agg := NewAggregator(nil, sqlStore)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		agg.Publish(mkRecord(base.Add(time.Duration(i)*time.Second), "c", "m", StatusSuccess, 1, 1))
	}

	dagg := agg.(*DefaultAggregator)
	if !dagg.waitForDrain(2 * time.Second) {
		t.Fatalf("worker did not drain")
	}

	res, err := agg.Query(context.Background(), Filter{}, Window{Start: base, End: base.Add(time.Hour)})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if res.TotalRequests != 5 {
		t.Errorf("sql-only TotalRequests = %d, want 5", res.TotalRequests)
	}
	if err := agg.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}
