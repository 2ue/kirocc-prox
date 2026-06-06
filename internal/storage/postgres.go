package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/niuma/kirocc-pro/internal/auth"
	"github.com/niuma/kirocc-pro/internal/pool"
)

func OpenPostgres(ctx context.Context, dsn string) (*sql.DB, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(30 * time.Minute)
	if err := pingPostgresWithRetry(ctx, db, 30*time.Second); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	if err := MigratePostgres(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func pingPostgresWithRetry(ctx context.Context, db *sql.DB, maxWait time.Duration) error {
	deadline := time.Now().Add(maxWait)
	delay := 200 * time.Millisecond
	var lastErr error
	for {
		if err := db.PingContext(ctx); err != nil {
			lastErr = err
		} else {
			return nil
		}
		if maxWait <= 0 || time.Now().Add(delay).After(deadline) {
			return lastErr
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		if delay < 2*time.Second {
			delay *= 2
			if delay > 2*time.Second {
				delay = 2 * time.Second
			}
		}
	}
}

func MigratePostgres(ctx context.Context, db *sql.DB) error {
	const schema = `
CREATE TABLE IF NOT EXISTS accounts (
  id text PRIMARY KEY,
  provider text NOT NULL DEFAULT 'kiro',
  label text NOT NULL DEFAULT '',
  priority integer NOT NULL DEFAULT 100,
  disabled boolean NOT NULL DEFAULT false,
  disabled_reason text NOT NULL DEFAULT '',
  disabled_at timestamptz,
  disable_cooling boolean NOT NULL DEFAULT false,
  max_in_flight integer NOT NULL DEFAULT 0,
  proxy_url text NOT NULL DEFAULT '',
  region text NOT NULL DEFAULT '',
  sso_region text NOT NULL DEFAULT '',
  profile_arn text NOT NULL DEFAULT '',
  auth_type text NOT NULL DEFAULT 'social',
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS account_credentials (
  account_id text PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
  access_token text NOT NULL DEFAULT '',
  refresh_token text NOT NULL DEFAULT '',
  client_id text NOT NULL DEFAULT '',
  client_secret text NOT NULL DEFAULT '',
  expires_at timestamptz,
  updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS account_quota_snapshots (
  account_id text PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
  fetched_at timestamptz,
  plan_name text NOT NULL DEFAULT '',
  plan_tier text NOT NULL DEFAULT '',
  credits_total double precision NOT NULL DEFAULT 0,
  credits_used double precision NOT NULL DEFAULT 0,
  bonus_total double precision NOT NULL DEFAULT 0,
  bonus_used double precision NOT NULL DEFAULT 0,
  bonus_expire_days integer NOT NULL DEFAULT 0,
  next_reset_at timestamptz,
  banned boolean NOT NULL DEFAULT false,
  ban_reason text NOT NULL DEFAULT '',
  last_quota_error text NOT NULL DEFAULT '',
  last_quota_error_at timestamptz,
  updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS settings_docs (
  key text PRIMARY KEY,
  data jsonb NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS usage_records (
  id bigserial PRIMARY KEY,
  ts timestamptz NOT NULL,
  cred_id text NOT NULL DEFAULT '',
  provider text NOT NULL DEFAULT '',
  request_path text NOT NULL DEFAULT '',
  prompt_cache_profile text NOT NULL DEFAULT '',
  prompt_cache_prefix text NOT NULL DEFAULT '',
  requested_model text NOT NULL DEFAULT '',
  resolved_model text NOT NULL DEFAULT '',
  input_tokens bigint NOT NULL DEFAULT 0,
  output_tokens bigint NOT NULL DEFAULT 0,
  cache_read_tokens bigint NOT NULL DEFAULT 0,
  cache_write_tokens bigint NOT NULL DEFAULT 0,
  raw_input_tokens bigint NOT NULL DEFAULT 0,
  raw_output_tokens bigint NOT NULL DEFAULT 0,
  raw_cache_read_tokens bigint NOT NULL DEFAULT 0,
  raw_cache_write_tokens bigint NOT NULL DEFAULT 0,
  status text NOT NULL DEFAULT '',
  latency_ms integer NOT NULL DEFAULT 0,
  first_token_ms integer NOT NULL DEFAULT 0,
  trace_id text NOT NULL DEFAULT '',
  error_message text NOT NULL DEFAULT '',
  req_type text NOT NULL DEFAULT '',
  device text NOT NULL DEFAULT '',
  device_id text NOT NULL DEFAULT '',
  api_key_id text NOT NULL DEFAULT '',
  credits_used_snapshot double precision NOT NULL DEFAULT 0,
  credits_total_snapshot double precision NOT NULL DEFAULT 0,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_usage_ts ON usage_records(ts DESC);
CREATE INDEX IF NOT EXISTS idx_usage_cred_ts ON usage_records(cred_id, ts DESC);
CREATE INDEX IF NOT EXISTS idx_usage_api_key_ts ON usage_records(api_key_id, ts DESC);
CREATE INDEX IF NOT EXISTS idx_usage_device_ts ON usage_records(device_id, ts DESC);
CREATE INDEX IF NOT EXISTS idx_usage_model_ts ON usage_records(resolved_model, ts DESC);
CREATE INDEX IF NOT EXISTS idx_usage_status_ts ON usage_records(status, ts DESC);
`
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("migrate postgres: %w", err)
	}
	return nil
}

type PostgresAccountStore struct {
	db *sql.DB
}

func NewPostgresAccountStore(db *sql.DB) *PostgresAccountStore {
	return &PostgresAccountStore{db: db}
}

func (s *PostgresAccountStore) Load(ctx context.Context) ([]*pool.Credential, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT
  a.id, a.provider, a.label, a.priority, a.disabled, a.disabled_reason, a.disabled_at,
  a.disable_cooling, a.max_in_flight, a.proxy_url, a.region, a.sso_region, a.profile_arn, a.auth_type,
  c.access_token, c.refresh_token, c.client_id, c.client_secret, c.expires_at,
  q.fetched_at, q.plan_name, q.plan_tier, q.credits_total, q.credits_used,
  q.bonus_total, q.bonus_used, q.bonus_expire_days, q.next_reset_at, q.banned, q.ban_reason,
  q.last_quota_error, q.last_quota_error_at
FROM accounts a
LEFT JOIN account_credentials c ON c.account_id = a.id
LEFT JOIN account_quota_snapshots q ON q.account_id = a.id
ORDER BY a.priority DESC, a.id ASC`)
	if err != nil {
		return nil, fmt.Errorf("load accounts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*pool.Credential
	for rows.Next() {
		var (
			id, provider, label, disabledReason, proxyURL string
			region, ssoRegion, profileARN, authType       string
			priority, maxInFlight                         int
			disabled, disableCooling                      bool
			disabledAt                                    sql.NullTime
			accessToken, refreshToken, clientID, secret   sql.NullString
			expiresAt                                     sql.NullTime
			qFetched, qNextReset, qErrAt                  sql.NullTime
			qPlan, qTier, qBanReason, qErr                sql.NullString
			qTotal, qUsed, qBonusTotal, qBonusUsed        sql.NullFloat64
			qBonusDays                                    sql.NullInt64
			qBanned                                       sql.NullBool
		)
		if err := rows.Scan(
			&id, &provider, &label, &priority, &disabled, &disabledReason, &disabledAt,
			&disableCooling, &maxInFlight, &proxyURL, &region, &ssoRegion, &profileARN, &authType,
			&accessToken, &refreshToken, &clientID, &secret, &expiresAt,
			&qFetched, &qPlan, &qTier, &qTotal, &qUsed, &qBonusTotal, &qBonusUsed,
			&qBonusDays, &qNextReset, &qBanned, &qBanReason, &qErr, &qErrAt,
		); err != nil {
			return nil, fmt.Errorf("scan account: %w", err)
		}
		expUnix := int64(0)
		if expiresAt.Valid {
			expUnix = expiresAt.Time.Unix()
		}
		c := &pool.Credential{
			ID:             id,
			Label:          label,
			Provider:       provider,
			Priority:       priority,
			Disabled:       disabled,
			DisableCooling: disableCooling,
			MaxInFlight:    maxInFlight,
			ProxyURL:       proxyURL,
			Credentials: auth.Credentials{
				AccessToken:  accessToken.String,
				RefreshToken: refreshToken.String,
				ClientID:     clientID.String,
				ClientSecret: secret.String,
				ExpiresAt:    expUnix,
				Region:       region,
				SSORegion:    ssoRegion,
				ProfileARN:   profileARN,
				AuthType:     authType,
			},
			DisabledReason: disabledReason,
		}
		if disabledAt.Valid {
			c.DisabledAt = disabledAt.Time
		}
		if qFetched.Valid || qPlan.Valid || qTotal.Valid || qUsed.Valid {
			c.LastQuota = &pool.KiroQuotaSnapshot{
				FetchedAt:       qFetched.Time,
				PlanName:        qPlan.String,
				PlanTier:        qTier.String,
				CreditsTotal:    qTotal.Float64,
				CreditsUsed:     qUsed.Float64,
				BonusTotal:      qBonusTotal.Float64,
				BonusUsed:       qBonusUsed.Float64,
				BonusExpireDays: int(qBonusDays.Int64),
				NextResetAt:     qNextReset.Time,
				Banned:          qBanned.Bool,
				BanReason:       qBanReason.String,
			}
			c.LastQuotaAt = qFetched.Time
		}
		c.LastQuotaError = qErr.String
		if qErrAt.Valid {
			c.LastQuotaErrorAt = qErrAt.Time
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate accounts: %w", err)
	}
	return out, nil
}

func (s *PostgresAccountStore) SaveAll(ctx context.Context, creds []*pool.Credential) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin save accounts: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	ids := make([]string, 0, len(creds))
	for _, c := range creds {
		if c == nil || c.ID == "" {
			continue
		}
		ids = append(ids, c.ID)
		if err := saveCredentialTx(ctx, tx, c); err != nil {
			return err
		}
	}
	if len(ids) == 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM accounts`); err != nil {
			return fmt.Errorf("delete all accounts: %w", err)
		}
	} else {
		args := make([]any, len(ids))
		holders := make([]string, len(ids))
		for i, id := range ids {
			args[i] = id
			holders[i] = fmt.Sprintf("$%d", i+1)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM accounts WHERE id NOT IN (`+strings.Join(holders, ",")+`)`, args...); err != nil {
			return fmt.Errorf("delete removed accounts: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit save accounts: %w", err)
	}
	return nil
}

func (s *PostgresAccountStore) SaveOne(ctx context.Context, cred *pool.Credential) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin save account: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := saveCredentialTx(ctx, tx, cred); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit save account: %w", err)
	}
	return nil
}

func (s *PostgresAccountStore) Delete(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM accounts WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete account %s: %w", id, err)
	}
	return nil
}

func saveCredentialTx(ctx context.Context, tx *sql.Tx, c *pool.Credential) error {
	if c == nil || c.ID == "" {
		return nil
	}
	c.Mu.RLock()
	expiresAt := sql.NullTime{}
	if c.ExpiresAt > 0 {
		expiresAt = sql.NullTime{Time: time.Unix(c.ExpiresAt, 0), Valid: true}
	}
	disabledAt := sql.NullTime{}
	if !c.DisabledAt.IsZero() {
		disabledAt = sql.NullTime{Time: c.DisabledAt, Valid: true}
	}
	provider := c.Provider
	if provider == "" {
		provider = "kiro"
	}
	priority := c.Priority
	if priority == 0 {
		priority = 100
	}
	authType := c.AuthType
	if authType == "" {
		authType = "social"
	}
	_, err := tx.ExecContext(ctx, `
INSERT INTO accounts (
  id, provider, label, priority, disabled, disabled_reason, disabled_at,
  disable_cooling, max_in_flight, proxy_url, region, sso_region, profile_arn, auth_type, updated_at
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,now())
ON CONFLICT (id) DO UPDATE SET
  provider=EXCLUDED.provider,
  label=EXCLUDED.label,
  priority=EXCLUDED.priority,
  disabled=EXCLUDED.disabled,
  disabled_reason=EXCLUDED.disabled_reason,
  disabled_at=EXCLUDED.disabled_at,
  disable_cooling=EXCLUDED.disable_cooling,
  max_in_flight=EXCLUDED.max_in_flight,
  proxy_url=EXCLUDED.proxy_url,
  region=EXCLUDED.region,
  sso_region=EXCLUDED.sso_region,
  profile_arn=EXCLUDED.profile_arn,
  auth_type=EXCLUDED.auth_type,
  updated_at=now()`,
		c.ID, provider, c.Label, priority, c.Disabled, c.DisabledReason, disabledAt,
		c.DisableCooling, c.MaxInFlight, c.ProxyURL, c.Region, c.SSORegion, c.ProfileARN, authType,
	)
	if err != nil {
		c.Mu.RUnlock()
		return fmt.Errorf("upsert account %s: %w", c.ID, err)
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO account_credentials (
  account_id, access_token, refresh_token, client_id, client_secret, expires_at, updated_at
) VALUES ($1,$2,$3,$4,$5,$6,now())
ON CONFLICT (account_id) DO UPDATE SET
  access_token=EXCLUDED.access_token,
  refresh_token=EXCLUDED.refresh_token,
  client_id=EXCLUDED.client_id,
  client_secret=EXCLUDED.client_secret,
  expires_at=EXCLUDED.expires_at,
  updated_at=now()`,
		c.ID, c.AccessToken, c.RefreshToken, c.ClientID, c.ClientSecret, expiresAt,
	)
	if err != nil {
		c.Mu.RUnlock()
		return fmt.Errorf("upsert account credentials %s: %w", c.ID, err)
	}
	if c.LastQuota != nil {
		q := c.LastQuota
		_, err = tx.ExecContext(ctx, `
INSERT INTO account_quota_snapshots (
  account_id, fetched_at, plan_name, plan_tier, credits_total, credits_used,
  bonus_total, bonus_used, bonus_expire_days, next_reset_at, banned, ban_reason,
  last_quota_error, last_quota_error_at, updated_at
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,now())
ON CONFLICT (account_id) DO UPDATE SET
  fetched_at=EXCLUDED.fetched_at,
  plan_name=EXCLUDED.plan_name,
  plan_tier=EXCLUDED.plan_tier,
  credits_total=EXCLUDED.credits_total,
  credits_used=EXCLUDED.credits_used,
  bonus_total=EXCLUDED.bonus_total,
  bonus_used=EXCLUDED.bonus_used,
  bonus_expire_days=EXCLUDED.bonus_expire_days,
  next_reset_at=EXCLUDED.next_reset_at,
  banned=EXCLUDED.banned,
  ban_reason=EXCLUDED.ban_reason,
  last_quota_error=EXCLUDED.last_quota_error,
  last_quota_error_at=EXCLUDED.last_quota_error_at,
  updated_at=now()`,
			c.ID, q.FetchedAt, q.PlanName, q.PlanTier, q.CreditsTotal, q.CreditsUsed,
			q.BonusTotal, q.BonusUsed, q.BonusExpireDays, nullableTime(q.NextResetAt),
			q.Banned, q.BanReason, c.LastQuotaError, nullableTime(c.LastQuotaErrorAt),
		)
		if err != nil {
			c.Mu.RUnlock()
			return fmt.Errorf("upsert quota snapshot %s: %w", c.ID, err)
		}
	}
	c.Mu.RUnlock()
	return nil
}

func nullableTime(t time.Time) sql.NullTime {
	if t.IsZero() {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: t, Valid: true}
}
