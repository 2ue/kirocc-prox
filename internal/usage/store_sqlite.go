package usage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const sqliteSchema = `
CREATE TABLE IF NOT EXISTS usage_records (
  ts                     INTEGER NOT NULL,
  cred_id                TEXT NOT NULL,
  provider               TEXT NOT NULL,
  requested_model        TEXT NOT NULL,
  resolved_model         TEXT NOT NULL,
  input_tokens           INTEGER NOT NULL,
  output_tokens          INTEGER NOT NULL,
  cache_read_tokens      INTEGER NOT NULL,
  cache_write_tokens     INTEGER NOT NULL,
  status                 TEXT NOT NULL,
  latency_ms             INTEGER NOT NULL,
  trace_id               TEXT NOT NULL,
  req_type               TEXT NOT NULL DEFAULT '',
  device                 TEXT NOT NULL DEFAULT '',
  device_id              TEXT NOT NULL DEFAULT '',
  api_key_id             TEXT NOT NULL DEFAULT '',
  credits_used_snapshot  REAL NOT NULL DEFAULT 0,
  credits_total_snapshot REAL NOT NULL DEFAULT 0
);
`

// sqliteIndexes runs after migrationColumns so indexes never reference
// columns that an older table hasn't received yet via ALTER TABLE.
const sqliteIndexes = `
CREATE INDEX IF NOT EXISTS idx_usage_ts          ON usage_records(ts);
CREATE INDEX IF NOT EXISTS idx_usage_cred_ts     ON usage_records(cred_id, ts);
CREATE INDEX IF NOT EXISTS idx_usage_api_key_ts  ON usage_records(api_key_id, ts);
CREATE INDEX IF NOT EXISTS idx_usage_device_ts   ON usage_records(device_id, ts);
`

// migrationColumns lists columns that may be missing from a pre-existing
// usage_records table (created by an earlier kirocc-pro version) and need
// to be added via ALTER TABLE on startup.
var migrationColumns = []struct{ name, def string }{
	{"req_type", "TEXT NOT NULL DEFAULT ''"},
	{"device", "TEXT NOT NULL DEFAULT ''"},
	{"device_id", "TEXT NOT NULL DEFAULT ''"},
	{"api_key_id", "TEXT NOT NULL DEFAULT ''"},
	{"credits_used_snapshot", "REAL NOT NULL DEFAULT 0"},
	{"credits_total_snapshot", "REAL NOT NULL DEFAULT 0"},
}

// SQLiteStore is an append-log Store backed by modernc.org/sqlite.
type SQLiteStore struct {
	db         *sql.DB
	insertStmt *sql.Stmt
}

// NewSQLiteStore opens (or creates) a SQLite database at path and ensures
// the schema is present.
func NewSQLiteStore(path string) (*SQLiteStore, error) {
	// _busy_timeout serializes concurrent writers without spurious
	// SQLITE_BUSY errors. We also cap the pool at 1 connection because
	// SQLite's default rollback journal does not support concurrent writes.
	dsn := path + "?_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if _, err := db.Exec(sqliteSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	// [fork] Apply backwards-compat ALTER TABLE for the history-panel
	// columns when an older database lacks them.
	for _, col := range migrationColumns {
		has, err := columnExists(db, "usage_records", col.name)
		if err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("probe column %s: %w", col.name, err)
		}
		if has {
			continue
		}
		if _, err := db.Exec(`ALTER TABLE usage_records ADD COLUMN ` + col.name + ` ` + col.def); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("add column %s: %w", col.name, err)
		}
	}
	if _, err := db.Exec(sqliteIndexes); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply indexes: %w", err)
	}
	stmt, err := db.Prepare(`INSERT INTO usage_records (
		ts, cred_id, provider, requested_model, resolved_model,
		input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
		status, latency_ms, trace_id,
		req_type, device, device_id, api_key_id,
		credits_used_snapshot, credits_total_snapshot
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("prepare insert: %w", err)
	}
	return &SQLiteStore{db: db, insertStmt: stmt}, nil
}

func columnExists(db *sql.DB, table, name string) (bool, error) {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			cid     int
			cname   string
			ctype   string
			notnull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &cname, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if cname == name {
			return true, nil
		}
	}
	return false, rows.Err()
}

// Append writes rec to the append log.
func (s *SQLiteStore) Append(rec Record) error {
	_, err := s.insertStmt.Exec(
		rec.Timestamp.UnixNano(),
		rec.CredentialID,
		rec.Provider,
		rec.RequestedModel,
		rec.ResolvedModel,
		rec.InputTokens,
		rec.OutputTokens,
		rec.CacheReadTokens,
		rec.CacheWriteTokens,
		rec.Status,
		rec.LatencyMs,
		rec.TraceID,
		rec.Type,
		rec.Device,
		rec.DeviceID,
		rec.APIKeyID,
		rec.CreditsUsedSnapshot,
		rec.CreditsTotalSnapshot,
	)
	if err != nil {
		return fmt.Errorf("insert usage: %w", err)
	}
	return nil
}

// Recent returns the most recent records matching filter, ordered by ts desc.
func (s *SQLiteStore) Recent(ctx context.Context, filter Filter, limit int) ([]Record, error) {
	if limit <= 0 {
		return nil, nil
	}
	var (
		clauses []string
		args    []any
	)
	if len(filter.CredentialIDs) > 0 {
		clauses = append(clauses, "cred_id IN ("+placeholders(len(filter.CredentialIDs))+")")
		for _, v := range filter.CredentialIDs {
			args = append(args, v)
		}
	}
	if len(filter.Models) > 0 {
		clauses = append(clauses, "resolved_model IN ("+placeholders(len(filter.Models))+")")
		for _, v := range filter.Models {
			args = append(args, v)
		}
	}
	if len(filter.Statuses) > 0 {
		clauses = append(clauses, "status IN ("+placeholders(len(filter.Statuses))+")")
		for _, v := range filter.Statuses {
			args = append(args, v)
		}
	}
	if len(filter.APIKeyIDs) > 0 {
		clauses = append(clauses, "api_key_id IN ("+placeholders(len(filter.APIKeyIDs))+")")
		for _, v := range filter.APIKeyIDs {
			args = append(args, v)
		}
	}
	if len(filter.DeviceIDs) > 0 {
		clauses = append(clauses, "device_id IN ("+placeholders(len(filter.DeviceIDs))+")")
		for _, v := range filter.DeviceIDs {
			args = append(args, v)
		}
	}
	where := ""
	if len(clauses) > 0 {
		where = "WHERE " + strings.Join(clauses, " AND ") + " "
	}
	q := `SELECT ts, cred_id, provider, requested_model, resolved_model,
		input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
		status, latency_ms, trace_id, req_type, device, device_id, api_key_id,
		credits_used_snapshot, credits_total_snapshot
		FROM usage_records ` + where + `ORDER BY ts DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query recent: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]Record, 0, limit)
	for rows.Next() {
		var (
			ts                          int64
			credID, provider            string
			reqModel, resModel          string
			in, out2, cr, cw, lat       int
			status, traceID, typ, dev   string
			devID, apiKeyID             string
			creditsUsed, creditsTotal   float64
		)
		if err := rows.Scan(&ts, &credID, &provider, &reqModel, &resModel,
			&in, &out2, &cr, &cw, &status, &lat, &traceID,
			&typ, &dev, &devID, &apiKeyID, &creditsUsed, &creditsTotal); err != nil {
			return nil, fmt.Errorf("scan recent: %w", err)
		}
		out = append(out, Record{
			Timestamp:            time.Unix(0, ts),
			CredentialID:         credID,
			Provider:             provider,
			RequestedModel:       reqModel,
			ResolvedModel:        resModel,
			InputTokens:          in,
			OutputTokens:         out2,
			CacheReadTokens:      cr,
			CacheWriteTokens:     cw,
			Status:               status,
			LatencyMs:            lat,
			TraceID:              traceID,
			Type:                 typ,
			Device:               dev,
			DeviceID:             devID,
			APIKeyID:             apiKeyID,
			CreditsUsedSnapshot:  creditsUsed,
			CreditsTotalSnapshot: creditsTotal,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recent: %w", err)
	}
	return out, nil
}

// Query rolls up records matching filter within window.
func (s *SQLiteStore) Query(ctx context.Context, filter Filter, window Window) (Aggregate, error) {
	agg := Aggregate{ByCredModel: make(map[string]map[string]CellStats)}

	var (
		clauses []string
		args    []any
	)
	clauses = append(clauses, "ts >= ?", "ts < ?")
	args = append(args, window.Start.UnixNano(), window.End.UnixNano())

	if len(filter.CredentialIDs) > 0 {
		clauses = append(clauses, "cred_id IN ("+placeholders(len(filter.CredentialIDs))+")")
		for _, v := range filter.CredentialIDs {
			args = append(args, v)
		}
	}
	if len(filter.Models) > 0 {
		clauses = append(clauses, "resolved_model IN ("+placeholders(len(filter.Models))+")")
		for _, v := range filter.Models {
			args = append(args, v)
		}
	}
	if len(filter.Statuses) > 0 {
		clauses = append(clauses, "status IN ("+placeholders(len(filter.Statuses))+")")
		for _, v := range filter.Statuses {
			args = append(args, v)
		}
	}
	if len(filter.APIKeyIDs) > 0 {
		clauses = append(clauses, "api_key_id IN ("+placeholders(len(filter.APIKeyIDs))+")")
		for _, v := range filter.APIKeyIDs {
			args = append(args, v)
		}
	}
	if len(filter.DeviceIDs) > 0 {
		clauses = append(clauses, "device_id IN ("+placeholders(len(filter.DeviceIDs))+")")
		for _, v := range filter.DeviceIDs {
			args = append(args, v)
		}
	}

	where := "WHERE " + strings.Join(clauses, " AND ")

	rowsQ := `SELECT ts, cred_id, resolved_model, input_tokens, output_tokens,
		cache_read_tokens, cache_write_tokens, status, api_key_id, device_id, latency_ms, device
		FROM usage_records ` + where
	rows, err := s.db.QueryContext(ctx, rowsQ, args...)
	if err != nil {
		return agg, fmt.Errorf("query usage: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			ts                  int64
			credID, model       string
			in, out, cr, cw     int
			status, akID, devID string
			lat                 int
			device              string
		)
		if err := rows.Scan(&ts, &credID, &model, &in, &out, &cr, &cw, &status, &akID, &devID, &lat, &device); err != nil {
			return agg, fmt.Errorf("scan usage: %w", err)
		}
		rec := Record{
			Timestamp:        time.Unix(0, ts),
			CredentialID:     credID,
			ResolvedModel:    model,
			InputTokens:      in,
			OutputTokens:     out,
			CacheReadTokens:  cr,
			CacheWriteTokens: cw,
			Status:           status,
			APIKeyID:         akID,
			DeviceID:         devID,
			LatencyMs:        lat,
			Device:           device,
		}
		applyToAggregate(&agg, rec)
	}
	if err := rows.Err(); err != nil {
		return agg, fmt.Errorf("iterate usage: %w", err)
	}

	// Timeline via GROUP BY on bucket-floored ts.
	if window.Bucket > 0 && !window.End.Before(window.Start) {
		bucketNanos := window.Bucket.Nanoseconds()
		startNanos := window.Start.UnixNano()
		duration := window.End.Sub(window.Start)
		n := int((duration + window.Bucket - 1) / window.Bucket)
		if n < 0 {
			n = 0
		}
		timeline := make([]TimelineBucket, n)
		for i := 0; i < n; i++ {
			timeline[i].Start = window.Start.Add(time.Duration(i) * window.Bucket)
		}

		// Anchor buckets to window.Start so off-grid windows still align as specified.
		tlQ := `SELECT ((ts - ?) / ?) AS bucket_idx,
			COUNT(*) AS reqs,
			SUM(CASE WHEN status = ? THEN 1 ELSE 0 END) AS succ,
			SUM(CASE WHEN status != ? THEN 1 ELSE 0 END) AS fail,
			SUM(input_tokens) AS in_tok,
			SUM(output_tokens) AS out_tok,
			SUM(cache_read_tokens) AS cr_tok,
			SUM(cache_write_tokens) AS cw_tok
			FROM usage_records ` + where + ` GROUP BY bucket_idx`
		tlArgs := []any{startNanos, bucketNanos, StatusSuccess, StatusSuccess}
		tlArgs = append(tlArgs, args...)

		tlRows, err := s.db.QueryContext(ctx, tlQ, tlArgs...)
		if err != nil {
			return agg, fmt.Errorf("query timeline: %w", err)
		}
		defer func() { _ = tlRows.Close() }()
		for tlRows.Next() {
			var idx int64
			var reqs, succ, fail, inTok, outTok, crTok, cwTok int64
			if err := tlRows.Scan(&idx, &reqs, &succ, &fail, &inTok, &outTok, &crTok, &cwTok); err != nil {
				return agg, fmt.Errorf("scan timeline: %w", err)
			}
			if idx < 0 || int(idx) >= n {
				continue
			}
			b := &timeline[idx]
			b.Requests = reqs
			b.Success = succ
			b.Failed = fail
			b.InputTokens = inTok
			b.OutputTokens = outTok
			b.CacheReadTokens = crTok
			b.CacheWriteTokens = cwTok
		}
		if err := tlRows.Err(); err != nil {
			return agg, fmt.Errorf("iterate timeline: %w", err)
		}
		agg.Timeline = timeline
	}

	return agg, nil
}

// Close releases the prepared statement and the database handle.
func (s *SQLiteStore) Close() error {
	if s.insertStmt != nil {
		_ = s.insertStmt.Close()
	}
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	parts := make([]string, n)
	for i := range parts {
		parts[i] = "?"
	}
	return strings.Join(parts, ",")
}
