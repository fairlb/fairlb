package usage

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

type postingDB interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// PostingStore owns the public posting watermark used by usage aggregation.
type PostingStore struct{ db postingDB }

func NewPostingStore(pool *pgxpool.Pool) *PostingStore { return &PostingStore{db: pool} }

func (s *PostingStore) WithTx(tx pgx.Tx) *PostingStore { return &PostingStore{db: tx} }

func (s *PostingStore) Ensure(ctx context.Context, key string) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO posting_watermarks (key, watermark)
		VALUES ($1, '1970-01-01T00:00:00Z') ON CONFLICT (key) DO NOTHING`, key)
	return err
}

func (s *PostingStore) GetForUpdate(ctx context.Context, key string) (pgtype.Timestamptz, error) {
	var out pgtype.Timestamptz
	err := s.db.QueryRow(ctx, `SELECT watermark FROM posting_watermarks WHERE key = $1 FOR UPDATE`, key).Scan(&out)
	return out, err
}

func (s *PostingStore) Get(ctx context.Context, key string) (pgtype.Timestamptz, error) {
	var out pgtype.Timestamptz
	err := s.db.QueryRow(ctx, `SELECT watermark FROM posting_watermarks WHERE key = $1`, key).Scan(&out)
	return out, err
}

func (s *PostingStore) AggregationCursor(ctx context.Context, graceMinutes int32) (pgtype.Timestamptz, error) {
	var out pgtype.Timestamptz
	err := s.db.QueryRow(ctx, `
		SELECT date_trunc('hour', now() - make_interval(mins => $1::int), 'UTC')::timestamptz`, graceMinutes).Scan(&out)
	return out, err
}

func (s *PostingStore) Set(ctx context.Context, key string, watermark pgtype.Timestamptz) error {
	_, err := s.db.Exec(ctx, `UPDATE posting_watermarks SET watermark = $2 WHERE key = $1`, key, watermark)
	return err
}
