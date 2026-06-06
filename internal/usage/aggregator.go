package usage

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// writeBufferSize bounds the queue feeding the persistent store worker. A full
// buffer causes Publish to drop the persistent write (memory still gets it)
// and log a warning, so a slow database never blocks request hot paths.
const writeBufferSize = 4096

// DefaultAggregator pairs a MemoryStore (for fast recent queries) with a
// persistent Store (for arbitrary-window queries). Durable writes are funneled
// through a background worker so Publish stays non-blocking.
type DefaultAggregator struct {
	mem   *MemoryStore
	store Store

	writes  chan Record
	done    chan struct{}
	wg      sync.WaitGroup
	closeMu sync.Mutex
	closed  bool
}

// NewAggregator constructs a DefaultAggregator. Either mem or persistent may be
// nil (but not both); a nil persistent disables durable persistence and a nil mem
// disables the in-memory fast path.
func NewAggregator(mem *MemoryStore, persistent Store) Aggregator {
	a := &DefaultAggregator{
		mem:   mem,
		store: persistent,
		done:  make(chan struct{}),
	}
	if persistent != nil {
		a.writes = make(chan Record, writeBufferSize)
		a.wg.Add(1)
		go a.runWriter()
	}
	return a
}

// Publish records r. Memory append happens inline; durable append is sent
// to the worker via a buffered channel. Failures (including a full
// channel) are logged, never returned.
func (a *DefaultAggregator) Publish(r Record) {
	if a.mem != nil {
		_ = a.mem.Append(r)
	}
	if a.writes == nil {
		return
	}
	a.closeMu.Lock()
	if a.closed {
		a.closeMu.Unlock()
		return
	}
	select {
	case a.writes <- r:
	default:
		slog.Warn("usage: persistent write buffer full, dropping record", "trace_id", r.TraceID)
	}
	a.closeMu.Unlock()
}

// Query satisfies Aggregator. It prefers the in-memory store when the
// requested window is fully contained within memory's retained range;
// otherwise it falls back to the persistent store.
func (a *DefaultAggregator) Query(ctx context.Context, filter Filter, window Window) (Aggregate, error) {
	useMem := a.mem != nil && a.mem.Len() > 0 && !window.Start.Before(a.mem.Oldest())
	if useMem {
		return a.mem.Query(ctx, filter, window)
	}
	if a.store != nil {
		return a.store.Query(ctx, filter, window)
	}
	if a.mem != nil {
		// Memory-only mode, even if the window predates the oldest record.
		return a.mem.Query(ctx, filter, window)
	}
	return Aggregate{ByCredModel: map[string]map[string]CellStats{}}, errors.New("usage: no store configured")
}

// Recent satisfies Aggregator. Prefers the in-memory store; falls back to
// the persistent store when memory is empty or contains fewer matching records
// than requested (the persistent query may surface older history).
func (a *DefaultAggregator) Recent(ctx context.Context, filter Filter, limit int) ([]Record, error) {
	if a.mem != nil {
		out, err := a.mem.Recent(ctx, filter, limit)
		if err != nil {
			return nil, err
		}
		if len(out) >= limit || a.store == nil {
			return out, nil
		}
		// Memory had fewer than requested; supplement from the persistent
		// store if available. It returns the most recent N records globally;
		// dedup against what memory already returned.
		fromSQL, err := a.store.Recent(ctx, filter, limit)
		if err != nil {
			return out, nil // best effort
		}
		seen := make(map[string]struct{}, len(out))
		for _, r := range out {
			seen[r.TraceID] = struct{}{}
		}
		for _, r := range fromSQL {
			if _, ok := seen[r.TraceID]; ok {
				continue
			}
			out = append(out, r)
			if len(out) >= limit {
				break
			}
		}
		return out, nil
	}
	if a.store != nil {
		return a.store.Recent(ctx, filter, limit)
	}
	return nil, errors.New("usage: no store configured")
}

// Close stops the writer, drains pending records, and closes the
// persistent store handle.
func (a *DefaultAggregator) Close() error {
	a.closeMu.Lock()
	if a.closed {
		a.closeMu.Unlock()
		return nil
	}
	a.closed = true
	if a.writes != nil {
		close(a.writes)
	}
	a.closeMu.Unlock()

	if a.writes != nil {
		a.wg.Wait()
	}
	close(a.done)
	if a.store != nil {
		return a.store.Close()
	}
	return nil
}

func (a *DefaultAggregator) runWriter() {
	defer a.wg.Done()
	for rec := range a.writes {
		if err := a.store.Append(rec); err != nil {
			slog.WarnContext(context.Background(), "usage: persistent append failed",
				"trace_id", rec.TraceID, "err", err)
		}
	}
}

// flushTimeout is exposed for tests that want to wait for the worker
// to drain without closing the aggregator.
func (a *DefaultAggregator) waitForDrain(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(a.writes) == 0 {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return len(a.writes) == 0
}
