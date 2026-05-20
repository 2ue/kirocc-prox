package usage

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestSQLiteStore_RoundTrip(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "usage.sqlite")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = store.Close() }()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 50; i++ {
		rec := mkRecord(base.Add(time.Duration(i)*time.Second), "cred-1", "claude-opus-4.7", StatusSuccess, 5, 10)
		if err := store.Append(rec); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	agg, err := store.Query(context.Background(), Filter{}, Window{Start: base, End: base.Add(time.Hour)})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if agg.TotalRequests != 50 {
		t.Errorf("TotalRequests = %d, want 50", agg.TotalRequests)
	}
	if agg.TotalInputTokens != 250 {
		t.Errorf("TotalInputTokens = %d, want 250", agg.TotalInputTokens)
	}
	if agg.TotalOutputTokens != 500 {
		t.Errorf("TotalOutputTokens = %d, want 500", agg.TotalOutputTokens)
	}
}

func TestSQLiteStore_QueryFilters(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "usage.sqlite")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = store.Close() }()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	records := []Record{
		mkRecord(base, "cred-a", "model-x", StatusSuccess, 1, 1),
		mkRecord(base.Add(1*time.Second), "cred-a", "model-y", StatusRateLimited, 2, 2),
		mkRecord(base.Add(2*time.Second), "cred-b", "model-x", StatusSuccess, 3, 3),
		mkRecord(base.Add(3*time.Second), "cred-b", "model-y", StatusUpstreamError, 4, 4),
	}
	for _, r := range records {
		if err := store.Append(r); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	window := Window{Start: base, End: base.Add(time.Hour)}

	agg, err := store.Query(context.Background(), Filter{CredentialIDs: []string{"cred-a"}}, window)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if agg.TotalRequests != 2 {
		t.Errorf("cred-a: got %d, want 2", agg.TotalRequests)
	}

	agg, _ = store.Query(context.Background(), Filter{Models: []string{"model-x"}}, window)
	if agg.TotalRequests != 2 {
		t.Errorf("model-x: got %d, want 2", agg.TotalRequests)
	}

	agg, _ = store.Query(context.Background(), Filter{Statuses: []string{StatusSuccess}}, window)
	if agg.TotalSuccess != 2 || agg.TotalRequests != 2 {
		t.Errorf("success: success=%d total=%d, want 2/2", agg.TotalSuccess, agg.TotalRequests)
	}

	agg, _ = store.Query(context.Background(), Filter{
		CredentialIDs: []string{"cred-b"},
		Models:        []string{"model-y"},
	}, window)
	if agg.TotalRequests != 1 {
		t.Errorf("combined: got %d, want 1", agg.TotalRequests)
	}
}

func TestSQLiteStore_TimelineBucketing(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "usage.sqlite")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = store.Close() }()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 60; i++ {
		_ = store.Append(mkRecord(base.Add(time.Duration(i)*time.Minute), "c", "m", StatusSuccess, 1, 2))
	}
	agg, err := store.Query(context.Background(), Filter{}, Window{
		Start:  base,
		End:    base.Add(60 * time.Minute),
		Bucket: 10 * time.Minute,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(agg.Timeline) != 6 {
		t.Fatalf("len Timeline = %d, want 6", len(agg.Timeline))
	}
	for i, b := range agg.Timeline {
		if b.Requests != 10 {
			t.Errorf("bucket[%d].Requests = %d, want 10", i, b.Requests)
		}
		wantStart := base.Add(time.Duration(i) * 10 * time.Minute)
		if !b.Start.Equal(wantStart) {
			t.Errorf("bucket[%d].Start = %v, want %v", i, b.Start, wantStart)
		}
	}
}

func TestSQLiteStore_SchemaCreatedOnFirstOpen(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "fresh.sqlite")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Query should succeed against an empty (but well-formed) DB.
	agg, err := store.Query(context.Background(), Filter{}, Window{
		Start: time.Now().Add(-time.Hour),
		End:   time.Now(),
	})
	if err != nil {
		t.Fatalf("query empty: %v", err)
	}
	if agg.TotalRequests != 0 {
		t.Errorf("TotalRequests = %d, want 0", agg.TotalRequests)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestSQLiteStore_Persistence(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "persist.sqlite")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		_ = store.Append(mkRecord(base.Add(time.Duration(i)*time.Second), "c", "m", StatusSuccess, 1, 1))
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close 1: %v", err)
	}

	store2, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("open 2: %v", err)
	}
	defer func() { _ = store2.Close() }()
	agg, err := store2.Query(context.Background(), Filter{}, Window{Start: base, End: base.Add(time.Hour)})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if agg.TotalRequests != 10 {
		t.Errorf("after reopen TotalRequests = %d, want 10", agg.TotalRequests)
	}
}
