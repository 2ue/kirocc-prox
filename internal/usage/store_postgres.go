package usage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

type PostgresStore struct {
	db         *sql.DB
	insertStmt *sql.Stmt
}

func NewPostgresStore(db *sql.DB) (*PostgresStore, error) {
	stmt, err := db.Prepare(`INSERT INTO usage_records (
		ts, cred_id, provider, request_path, prompt_cache_profile, prompt_cache_prefix,
		requested_model, resolved_model,
		input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
		raw_input_tokens, raw_output_tokens, raw_cache_read_tokens, raw_cache_write_tokens,
		status, latency_ms, first_token_ms, trace_id, error_message,
		req_type, device, device_id, api_key_id,
		credits_used_snapshot, credits_total_snapshot
	) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27)`)
	if err != nil {
		return nil, fmt.Errorf("prepare postgres usage insert: %w", err)
	}
	return &PostgresStore{db: db, insertStmt: stmt}, nil
}

func (s *PostgresStore) Append(rec Record) error {
	_, err := s.insertStmt.Exec(
		rec.Timestamp,
		rec.CredentialID,
		rec.Provider,
		rec.RequestPath,
		rec.PromptCacheProfile,
		rec.PromptCachePrefix,
		rec.RequestedModel,
		rec.ResolvedModel,
		rec.InputTokens,
		rec.OutputTokens,
		rec.CacheReadTokens,
		rec.CacheWriteTokens,
		rec.RawInputTokens,
		rec.RawOutputTokens,
		rec.RawCacheReadTokens,
		rec.RawCacheWriteTokens,
		rec.Status,
		rec.LatencyMs,
		rec.FirstTokenMs,
		rec.TraceID,
		rec.ErrorMessage,
		rec.Type,
		rec.Device,
		rec.DeviceID,
		rec.APIKeyID,
		rec.CreditsUsedSnapshot,
		rec.CreditsTotalSnapshot,
	)
	if err != nil {
		return fmt.Errorf("insert postgres usage: %w", err)
	}
	return nil
}

func (s *PostgresStore) Recent(ctx context.Context, filter Filter, limit int) ([]Record, error) {
	if limit <= 0 {
		return nil, nil
	}
	where, args := postgresWhere(filter, 1)
	q := `SELECT ts, cred_id, provider, request_path, prompt_cache_profile, prompt_cache_prefix,
		requested_model, resolved_model,
		input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
		raw_input_tokens, raw_output_tokens, raw_cache_read_tokens, raw_cache_write_tokens,
		status, latency_ms, first_token_ms, trace_id, error_message, req_type, device, device_id, api_key_id,
		credits_used_snapshot, credits_total_snapshot
		FROM usage_records ` + where + ` ORDER BY ts DESC LIMIT $` + fmt.Sprint(len(args)+1)
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query postgres recent: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]Record, 0, limit)
	for rows.Next() {
		var (
			ts                          time.Time
			credID, provider            string
			requestPath, pcProfile      string
			pcPrefix                    string
			reqModel, resModel          string
			in, out2, cr, cw            int
			rawIn, rawOut, rawCR, rawCW int
			lat, firstToken             int
			status, traceID, typ, dev   string
			errMsg                      string
			devID, apiKeyID             string
			creditsUsed, creditsTotal   float64
		)
		if err := rows.Scan(&ts, &credID, &provider, &requestPath, &pcProfile, &pcPrefix,
			&reqModel, &resModel, &in, &out2, &cr, &cw,
			&rawIn, &rawOut, &rawCR, &rawCW,
			&status, &lat, &firstToken, &traceID, &errMsg,
			&typ, &dev, &devID, &apiKeyID, &creditsUsed, &creditsTotal); err != nil {
			return nil, fmt.Errorf("scan postgres recent: %w", err)
		}
		out = append(out, Record{
			Timestamp:            ts,
			CredentialID:         credID,
			Provider:             provider,
			RequestPath:          requestPath,
			PromptCacheProfile:   pcProfile,
			PromptCachePrefix:    pcPrefix,
			RequestedModel:       reqModel,
			ResolvedModel:        resModel,
			InputTokens:          in,
			OutputTokens:         out2,
			CacheReadTokens:      cr,
			CacheWriteTokens:     cw,
			RawInputTokens:       rawIn,
			RawOutputTokens:      rawOut,
			RawCacheReadTokens:   rawCR,
			RawCacheWriteTokens:  rawCW,
			Status:               status,
			LatencyMs:            lat,
			FirstTokenMs:         firstToken,
			TraceID:              traceID,
			ErrorMessage:         errMsg,
			Type:                 typ,
			Device:               dev,
			DeviceID:             devID,
			APIKeyID:             apiKeyID,
			CreditsUsedSnapshot:  creditsUsed,
			CreditsTotalSnapshot: creditsTotal,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate postgres recent: %w", err)
	}
	return out, nil
}

func (s *PostgresStore) Query(ctx context.Context, filter Filter, window Window) (Aggregate, error) {
	agg := Aggregate{ByCredModel: make(map[string]map[string]CellStats)}
	where, args := postgresWhere(filter, 3)
	args = append([]any{window.Start, window.End}, args...)
	prefix := "WHERE ts >= $1 AND ts < $2"
	if where != "" {
		prefix += " AND " + strings.TrimPrefix(where, "WHERE ")
	}

	rowsQ := `SELECT ts, cred_id, resolved_model, input_tokens, output_tokens,
		cache_read_tokens, cache_write_tokens, status, api_key_id, device_id, latency_ms, device
		FROM usage_records ` + prefix
	rows, err := s.db.QueryContext(ctx, rowsQ, args...)
	if err != nil {
		return agg, fmt.Errorf("query postgres usage: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			ts                  time.Time
			credID, model       string
			in, out, cr, cw     int
			status, akID, devID string
			lat                 int
			device              string
		)
		if err := rows.Scan(&ts, &credID, &model, &in, &out, &cr, &cw, &status, &akID, &devID, &lat, &device); err != nil {
			return agg, fmt.Errorf("scan postgres usage: %w", err)
		}
		applyToAggregate(&agg, Record{
			Timestamp:        ts,
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
		})
	}
	if err := rows.Err(); err != nil {
		return agg, fmt.Errorf("iterate postgres usage: %w", err)
	}

	if window.Bucket > 0 && !window.End.Before(window.Start) {
		duration := window.End.Sub(window.Start)
		n := int((duration + window.Bucket - 1) / window.Bucket)
		if n < 0 {
			n = 0
		}
		timeline := make([]TimelineBucket, n)
		for i := 0; i < n; i++ {
			timeline[i].Start = window.Start.Add(time.Duration(i) * window.Bucket)
		}
		bucketSeconds := window.Bucket.Seconds()
		tlWhere, tlFilterArgs := postgresWhere(filter, 5)
		tlPrefix := "WHERE ts >= $1 AND ts < $2"
		if tlWhere != "" {
			tlPrefix += " AND " + strings.TrimPrefix(tlWhere, "WHERE ")
		}
		tlQ := `SELECT floor(extract(epoch from (ts - $1::timestamptz)) / $3)::bigint AS bucket_idx,
			COUNT(*) AS reqs,
			SUM(CASE WHEN status = $4 THEN 1 ELSE 0 END) AS succ,
			SUM(CASE WHEN status != $4 THEN 1 ELSE 0 END) AS fail,
			SUM(input_tokens) AS in_tok,
			SUM(output_tokens) AS out_tok,
			SUM(cache_read_tokens) AS cr_tok,
			SUM(cache_write_tokens) AS cw_tok
			FROM usage_records ` + tlPrefix + ` GROUP BY bucket_idx`
		tlArgs := append([]any{window.Start, window.End, bucketSeconds, StatusSuccess}, tlFilterArgs...)
		tlRows, err := s.db.QueryContext(ctx, tlQ, tlArgs...)
		if err != nil {
			return agg, fmt.Errorf("query postgres timeline: %w", err)
		}
		defer func() { _ = tlRows.Close() }()
		for tlRows.Next() {
			var idx int64
			var reqs, succ, fail, inTok, outTok, crTok, cwTok int64
			if err := tlRows.Scan(&idx, &reqs, &succ, &fail, &inTok, &outTok, &crTok, &cwTok); err != nil {
				return agg, fmt.Errorf("scan postgres timeline: %w", err)
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
			return agg, fmt.Errorf("iterate postgres timeline: %w", err)
		}
		agg.Timeline = timeline
	}
	return agg, nil
}

func (s *PostgresStore) Close() error {
	if s.insertStmt != nil {
		_ = s.insertStmt.Close()
	}
	return nil
}

func postgresWhere(filter Filter, start int) (string, []any) {
	var clauses []string
	var args []any
	next := start
	addIn := func(col string, vals []string) {
		if len(vals) == 0 {
			return
		}
		holders := make([]string, len(vals))
		for i, v := range vals {
			holders[i] = fmt.Sprintf("$%d", next)
			next++
			args = append(args, v)
		}
		clauses = append(clauses, col+" IN ("+strings.Join(holders, ",")+")")
	}
	addIn("cred_id", filter.CredentialIDs)
	addIn("resolved_model", filter.Models)
	addIn("status", filter.Statuses)
	addIn("api_key_id", filter.APIKeyIDs)
	addIn("device_id", filter.DeviceIDs)
	if len(clauses) == 0 {
		return "", args
	}
	return "WHERE " + strings.Join(clauses, " AND "), args
}
