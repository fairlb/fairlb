package apikeys

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

type storeDB interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// Store exposes the public API-key persistence operations used by other
// modules without exporting generated query implementations.
type Store struct{ db storeDB }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{db: pool} }

func (s *Store) WithTx(tx pgx.Tx) *Store { return &Store{db: tx} }

// Record is the persistence shape owned by the API-key domain. Keeping it here
// prevents callers from depending on a repository-wide generated model.
type Record struct {
	ID                 pgtype.UUID
	OrgID              pgtype.UUID
	Name               string
	Prefix             string
	KeyHash            string
	Scopes             []string
	AllowAllModels     bool
	AllowedModels      []string
	SpendLimitNano     pgtype.Int8
	SpendLimitInterval pgtype.Text
	RateLimitRpm       pgtype.Int4
	RateLimitTpm       pgtype.Int4
	Tags               []byte
	TotalSpentNano     int64
	Status             string
	LastUsedAt         pgtype.Timestamptz
	ExpiresAt          pgtype.Timestamptz
	CreatedAt          pgtype.Timestamptz
	UpdatedAt          pgtype.Timestamptz
}

const recordColumns = `id, org_id, name, prefix, key_hash, scopes,
	allow_all_models, allowed_models, spend_limit_nano, spend_limit_interval,
	rate_limit_rpm, rate_limit_tpm, tags, total_spent_nano, status, last_used_at,
	expires_at, created_at, updated_at`

func scanRecord(row pgx.Row) (Record, error) {
	var out Record
	err := row.Scan(
		&out.ID, &out.OrgID, &out.Name, &out.Prefix, &out.KeyHash, &out.Scopes,
		&out.AllowAllModels, &out.AllowedModels, &out.SpendLimitNano,
		&out.SpendLimitInterval, &out.RateLimitRpm, &out.RateLimitTpm, &out.Tags,
		&out.TotalSpentNano, &out.Status, &out.LastUsedAt, &out.ExpiresAt,
		&out.CreatedAt, &out.UpdatedAt,
	)
	return out, err
}

type InsertParams struct {
	OrgID              pgtype.UUID
	Name               string
	Prefix             string
	KeyHash            string
	SpendLimitNano     pgtype.Int8
	SpendLimitInterval pgtype.Text
	RateLimitRpm       pgtype.Int4
	RateLimitTpm       pgtype.Int4
	ExpiresAt          pgtype.Timestamptz
	AllowAllModels     bool
	AllowedModels      []string
	Tags               []byte
	Scopes             []string
}

func (s *Store) Insert(ctx context.Context, in InsertParams) (Record, error) {
	return scanRecord(s.db.QueryRow(ctx, `
		INSERT INTO api_keys (org_id, name, prefix, key_hash, spend_limit_nano,
			spend_limit_interval, rate_limit_rpm, rate_limit_tpm, expires_at,
			allow_all_models, allowed_models, tags, scopes)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11,
			COALESCE($12::jsonb, '[]'::jsonb),
			COALESCE(NULLIF($13::text[], '{}'), ARRAY['inference']))
		RETURNING `+recordColumns,
		in.OrgID, in.Name, in.Prefix, in.KeyHash, in.SpendLimitNano,
		in.SpendLimitInterval, in.RateLimitRpm, in.RateLimitTpm, in.ExpiresAt,
		in.AllowAllModels, in.AllowedModels, in.Tags, in.Scopes))
}

func (s *Store) RecordByHash(ctx context.Context, keyHash string) (Record, error) {
	return scanRecord(s.db.QueryRow(ctx, `SELECT `+recordColumns+` FROM api_keys WHERE key_hash = $1`, keyHash))
}

func (s *Store) RecordByOrg(ctx context.Context, keyID, orgID pgtype.UUID) (Record, error) {
	return scanRecord(s.db.QueryRow(ctx, `SELECT `+recordColumns+` FROM api_keys WHERE id = $1 AND org_id = $2`, keyID, orgID))
}

func (s *Store) ListRecordsByOrg(
	ctx context.Context,
	orgID pgtype.UUID,
	limit int32,
	cursorCreatedAt pgtype.Timestamptz,
	cursorID pgtype.UUID,
) ([]Record, error) {
	rows, err := s.db.Query(ctx, `SELECT `+recordColumns+`
		FROM api_keys
		WHERE org_id = $1
		  AND ($3::timestamptz IS NULL OR (created_at, id) < ($3::timestamptz, $4::uuid))
		ORDER BY created_at DESC, id DESC LIMIT $2`, orgID, limit, cursorCreatedAt, cursorID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Record, 0)
	for rows.Next() {
		record, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, rows.Err()
}

func (s *Store) Revoke(ctx context.Context, keyID, orgID pgtype.UUID) (int64, error) {
	tag, err := s.db.Exec(ctx, `
		UPDATE api_keys SET status = 'revoked'
		WHERE id = $1 AND org_id = $2 AND status = 'active'`, keyID, orgID)
	return tag.RowsAffected(), err
}

type UpdateControlsParams struct {
	ID                 pgtype.UUID
	OrgID              pgtype.UUID
	ClearSpendLimit    bool
	SpendLimitNano     pgtype.Int8
	SpendLimitInterval pgtype.Text
	ClearRateLimitRpm  bool
	RateLimitRpm       pgtype.Int4
	ClearRateLimitTpm  bool
	RateLimitTpm       pgtype.Int4
	ClearExpires       bool
	ExpiresAt          pgtype.Timestamptz
	SetModelAccess     bool
	AllowAllModels     bool
	AllowedModels      []string
	Tags               []byte
}

func (s *Store) UpdateControls(ctx context.Context, in UpdateControlsParams) (Record, error) {
	return scanRecord(s.db.QueryRow(ctx, `
		UPDATE api_keys SET
		  spend_limit_nano = CASE WHEN $3 THEN NULL WHEN $4::bigint IS NOT NULL THEN $4 ELSE spend_limit_nano END,
		  spend_limit_interval = CASE WHEN $3 THEN NULL WHEN $5::text IS NOT NULL THEN $5 ELSE spend_limit_interval END,
		  rate_limit_rpm = CASE WHEN $6 THEN NULL WHEN $7::integer IS NOT NULL THEN $7 ELSE rate_limit_rpm END,
		  rate_limit_tpm = CASE WHEN $8 THEN NULL WHEN $9::integer IS NOT NULL THEN $9 ELSE rate_limit_tpm END,
		  expires_at = CASE WHEN $10 THEN NULL WHEN $11::timestamptz IS NOT NULL THEN $11 ELSE expires_at END,
		  allow_all_models = CASE WHEN $12 THEN $13 ELSE allow_all_models END,
		  allowed_models = CASE WHEN $12 THEN $14::text[] ELSE allowed_models END,
		  tags = COALESCE($15::jsonb, tags), updated_at = now()
		WHERE id = $1 AND org_id = $2 AND status = 'active'
		RETURNING `+recordColumns,
		in.ID, in.OrgID, in.ClearSpendLimit, in.SpendLimitNano,
		in.SpendLimitInterval, in.ClearRateLimitRpm, in.RateLimitRpm,
		in.ClearRateLimitTpm, in.RateLimitTpm, in.ClearExpires, in.ExpiresAt,
		in.SetModelAccess, in.AllowAllModels, in.AllowedModels, in.Tags))
}

func (s *Store) TouchLastUsed(ctx context.Context, keyID pgtype.UUID) error {
	_, err := s.db.Exec(ctx, `UPDATE api_keys SET last_used_at = now() WHERE id = $1`, keyID)
	return err
}

func (s *Store) SpendSince(ctx context.Context, keyID pgtype.UUID, day pgtype.Date) (int64, error) {
	var total int64
	err := s.db.QueryRow(ctx, `
		SELECT COALESCE(SUM(spent_nano), 0)::bigint
		FROM api_key_daily_spend WHERE api_key_id = $1 AND day >= $2`, keyID, day).Scan(&total)
	return total, err
}

type Summary struct {
	ID         pgtype.UUID
	Name       string
	Prefix     string
	Status     string
	CreatedAt  pgtype.Timestamptz
	LastUsedAt pgtype.Timestamptz
}

type AuthKey struct {
	ID        pgtype.UUID
	OrgID     pgtype.UUID
	Scopes    []string
	Status    string
	ExpiresAt pgtype.Timestamptz
}

func (s *Store) KeyByHash(ctx context.Context, keyHash string) (AuthKey, error) {
	var out AuthKey
	err := s.db.QueryRow(ctx, `
		SELECT id, org_id, scopes, status, expires_at
		FROM api_keys WHERE key_hash = $1`, keyHash).
		Scan(&out.ID, &out.OrgID, &out.Scopes, &out.Status, &out.ExpiresAt)
	return out, err
}

func (s *Store) ListByOrg(ctx context.Context, orgID pgtype.UUID, limit int32) ([]Summary, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, name, prefix, status, created_at, last_used_at
		FROM api_keys WHERE org_id = $1
		ORDER BY created_at DESC, id DESC LIMIT $2`, orgID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Summary, 0)
	for rows.Next() {
		var row Summary
		if err := rows.Scan(&row.ID, &row.Name, &row.Prefix, &row.Status, &row.CreatedAt, &row.LastUsedAt); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// AddSpend updates the daily and all-time counters on one transaction-bound
// store, keeping budget enforcement consistent with billing settlement.
func (s *Store) AddSpend(ctx context.Context, keyID pgtype.UUID, day pgtype.Date, amount int64) error {
	if _, err := s.db.Exec(ctx, `
		INSERT INTO api_key_daily_spend (api_key_id, day, spent_nano)
		VALUES ($1, $2, $3)
		ON CONFLICT (api_key_id, day)
		DO UPDATE SET spent_nano = api_key_daily_spend.spent_nano + excluded.spent_nano`,
		keyID, day, amount); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `
		UPDATE api_keys SET total_spent_nano = total_spent_nano + $2 WHERE id = $1`,
		keyID, amount)
	return err
}
