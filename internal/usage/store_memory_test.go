package usage

import (
	"context"
	"testing"
	"time"
)

func mkRecord(ts time.Time, cred, model, status string, in, out int) Record {
	return Record{
		Timestamp:        ts,
		CredentialID:     cred,
		Provider:         "kiro",
		RequestedModel:   model,
		ResolvedModel:    model,
		InputTokens:      in,
		OutputTokens:     out,
		CacheReadTokens:  0,
		CacheWriteTokens: 0,
		Status:           status,
		LatencyMs:        10,
		TraceID:          "trace-" + cred,
	}
}

func TestMemoryStore_RoundTrip100(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore(1000)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 100; i++ {
		rec := mkRecord(base.Add(time.Duration(i)*time.Second), "cred-1", "claude-opus-4.7", StatusSuccess, 10, 20)
		if err := store.Append(rec); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if got := store.Len(); got != 100 {
		t.Fatalf("Len = %d, want 100", got)
	}
	window := Window{Start: base, End: base.Add(200 * time.Second)}
	agg, err := store.Query(context.Background(), Filter{}, window)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if agg.TotalRequests != 100 {
		t.Errorf("TotalRequests = %d, want 100", agg.TotalRequests)
	}
	if agg.TotalSuccess != 100 {
		t.Errorf("TotalSuccess = %d, want 100", agg.TotalSuccess)
	}
	if agg.TotalInputTokens != 1000 {
		t.Errorf("TotalInputTokens = %d, want 1000", agg.TotalInputTokens)
	}
	if agg.TotalOutputTokens != 2000 {
		t.Errorf("TotalOutputTokens = %d, want 2000", agg.TotalOutputTokens)
	}
	cell := agg.ByCredModel["cred-1"]["claude-opus-4.7"]
	if cell.Requests != 100 {
		t.Errorf("cell.Requests = %d, want 100", cell.Requests)
	}
}

func TestMemoryStore_CapacityOverflowDropsOldest(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore(5)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		rec := mkRecord(base.Add(time.Duration(i)*time.Second), "cred-1", "claude-opus-4.7", StatusSuccess, 1, 1)
		_ = store.Append(rec)
	}
	if got := store.Len(); got != 5 {
		t.Fatalf("Len = %d, want 5", got)
	}
	// Oldest should now be record #5 (the first 5 were overwritten).
	wantOldest := base.Add(5 * time.Second)
	if got := store.Oldest(); !got.Equal(wantOldest) {
		t.Errorf("Oldest = %v, want %v", got, wantOldest)
	}
	window := Window{Start: base, End: base.Add(time.Hour)}
	agg, _ := store.Query(context.Background(), Filter{}, window)
	if agg.TotalRequests != 5 {
		t.Errorf("TotalRequests after overflow = %d, want 5", agg.TotalRequests)
	}
}

func TestMemoryStore_QueryFilters(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore(100)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	_ = store.Append(mkRecord(base, "cred-a", "model-x", StatusSuccess, 1, 1))
	_ = store.Append(mkRecord(base.Add(1*time.Second), "cred-a", "model-y", StatusSuccess, 1, 1))
	_ = store.Append(mkRecord(base.Add(2*time.Second), "cred-b", "model-x", StatusRateLimited, 1, 1))
	_ = store.Append(mkRecord(base.Add(3*time.Second), "cred-b", "model-y", StatusUpstreamError, 1, 1))

	window := Window{Start: base, End: base.Add(time.Hour)}

	// Filter by credential.
	agg, _ := store.Query(context.Background(), Filter{CredentialIDs: []string{"cred-a"}}, window)
	if agg.TotalRequests != 2 {
		t.Errorf("cred-a filter: got %d, want 2", agg.TotalRequests)
	}

	// Filter by model.
	agg, _ = store.Query(context.Background(), Filter{Models: []string{"model-x"}}, window)
	if agg.TotalRequests != 2 {
		t.Errorf("model-x filter: got %d, want 2", agg.TotalRequests)
	}

	// Filter by status.
	agg, _ = store.Query(context.Background(), Filter{Statuses: []string{StatusRateLimited, StatusUpstreamError}}, window)
	if agg.TotalRequests != 2 {
		t.Errorf("error status filter: got %d, want 2", agg.TotalRequests)
	}
	if agg.TotalFailed != 2 {
		t.Errorf("TotalFailed = %d, want 2", agg.TotalFailed)
	}

	// Combined filter.
	agg, _ = store.Query(context.Background(), Filter{
		CredentialIDs: []string{"cred-b"},
		Models:        []string{"model-x"},
	}, window)
	if agg.TotalRequests != 1 {
		t.Errorf("combined filter: got %d, want 1", agg.TotalRequests)
	}
}

func TestMemoryStore_TimelineBucketing(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore(1000)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Spread 60 records over 60 minutes, one per minute.
	for i := 0; i < 60; i++ {
		rec := mkRecord(base.Add(time.Duration(i)*time.Minute), "cred-1", "m", StatusSuccess, 1, 2)
		_ = store.Append(rec)
	}
	window := Window{
		Start:  base,
		End:    base.Add(60 * time.Minute),
		Bucket: 10 * time.Minute,
	}
	agg, err := store.Query(context.Background(), Filter{}, window)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(agg.Timeline) != 6 {
		t.Fatalf("len Timeline = %d, want 6", len(agg.Timeline))
	}
	for i, b := range agg.Timeline {
		wantStart := base.Add(time.Duration(i) * 10 * time.Minute)
		if !b.Start.Equal(wantStart) {
			t.Errorf("bucket[%d].Start = %v, want %v", i, b.Start, wantStart)
		}
		if b.Requests != 10 {
			t.Errorf("bucket[%d].Requests = %d, want 10", i, b.Requests)
		}
		if b.InputTokens != 10 {
			t.Errorf("bucket[%d].InputTokens = %d, want 10", i, b.InputTokens)
		}
		if b.OutputTokens != 20 {
			t.Errorf("bucket[%d].OutputTokens = %d, want 20", i, b.OutputTokens)
		}
	}
}

func TestMemoryStore_TimelineNilWhenBucketZero(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore(10)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	_ = store.Append(mkRecord(base, "c", "m", StatusSuccess, 1, 1))
	agg, _ := store.Query(context.Background(), Filter{}, Window{Start: base, End: base.Add(time.Hour)})
	if agg.Timeline != nil {
		t.Errorf("Timeline = %v, want nil", agg.Timeline)
	}
}
