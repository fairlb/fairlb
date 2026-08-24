package httpx

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

type idempotencyDB interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type idempotencyStore struct{ db idempotencyDB }

func newIdempotencyStore(pool *pgxpool.Pool) *idempotencyStore {
	return &idempotencyStore{db: pool}
}

type idempotencyRow struct {
	RequestHash     string
	Status          string
	ResponseStatus  pgtype.Int4
	ResponseHeaders []byte
	ResponseBody    []byte
}

func (s *idempotencyStore) claim(
	ctx context.Context,
	scope string,
	key string,
	requestHash string,
	expiresAt pgtype.Timestamptz,
) error {
	var id pgtype.UUID
	return s.db.QueryRow(ctx, `
		INSERT INTO idempotency_keys (scope, idempotency_key, request_hash, expires_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (scope, idempotency_key) DO NOTHING
		RETURNING id`, scope, key, requestHash, expiresAt).Scan(&id)
}

func (s *idempotencyStore) get(ctx context.Context, scope, key string) (idempotencyRow, error) {
	var out idempotencyRow
	err := s.db.QueryRow(ctx, `
		SELECT request_hash, status, response_status, response_headers, response_body
		FROM idempotency_keys WHERE scope = $1 AND idempotency_key = $2`, scope, key).
		Scan(&out.RequestHash, &out.Status, &out.ResponseStatus, &out.ResponseHeaders, &out.ResponseBody)
	return out, err
}

func (s *idempotencyStore) takeOver(
	ctx context.Context,
	scope string,
	key string,
	requestHash string,
	expiresAt pgtype.Timestamptz,
	staleBefore pgtype.Timestamptz,
) error {
	var id pgtype.UUID
	return s.db.QueryRow(ctx, `
		UPDATE idempotency_keys SET status = 'in_flight', expires_at = $4
		WHERE scope = $1 AND idempotency_key = $2 AND request_hash = $3
		  AND status = 'in_flight' AND updated_at < $5
		RETURNING id`, scope, key, requestHash, expiresAt, staleBefore).Scan(&id)
}

func (s *idempotencyStore) complete(
	ctx context.Context,
	scope string,
	key string,
	requestHash string,
	status pgtype.Int4,
	headers []byte,
	body []byte,
) error {
	_, err := s.db.Exec(ctx, `
		UPDATE idempotency_keys
		SET status = 'completed', response_status = $3, response_headers = $4, response_body = $5
		WHERE scope = $1 AND idempotency_key = $2
		  AND status = 'in_flight' AND request_hash = $6`,
		scope, key, status, headers, body, requestHash)
	return err
}

func (s *idempotencyStore) vacate(ctx context.Context, scope, key, requestHash string) error {
	_, err := s.db.Exec(ctx, `
		DELETE FROM idempotency_keys
		WHERE scope = $1 AND idempotency_key = $2 AND request_hash = $3 AND status = 'in_flight'`,
		scope, key, requestHash)
	return err
}

func (s *idempotencyStore) deleteExpired(ctx context.Context) (int64, error) {
	tag, err := s.db.Exec(ctx, `DELETE FROM idempotency_keys WHERE expires_at < now()`)
	return tag.RowsAffected(), err
}
