// Package organizations owns writes to the public organizations table.
package organizations

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

type dbtx interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type Store struct{ db dbtx }

func New(pool *pgxpool.Pool) *Store { return &Store{db: pool} }

func (s *Store) WithTx(tx pgx.Tx) *Store { return &Store{db: tx} }

type Organization struct {
	ID        pgtype.UUID
	Slug      string
	Name      string
	Kind      string
	Status    string
	Currency  string
	CreatedAt pgtype.Timestamptz
	UpdatedAt pgtype.Timestamptz
}

type DataplaneSnapshot struct {
	Status   string
	Currency string
}

type CreateOrganization struct {
	Slug     string
	Name     string
	Kind     string
	Currency string
}

func (s *Store) Create(ctx context.Context, in CreateOrganization) (Organization, error) {
	if in.Currency == "" {
		in.Currency = "USD"
	}
	return scanOrganization(s.db.QueryRow(ctx, `
		INSERT INTO orgs (slug, name, kind, currency) VALUES ($1, $2, $3, $4)
		ON CONFLICT (slug) DO NOTHING
		RETURNING id, slug, name, kind, status, currency, created_at, updated_at`,
		in.Slug, in.Name, in.Kind, in.Currency))
}

func (s *Store) UpdateName(ctx context.Context, id pgtype.UUID, name string) (Organization, error) {
	return scanOrganization(s.db.QueryRow(ctx, `
		UPDATE orgs SET name = $2 WHERE id = $1
		RETURNING id, slug, name, kind, status, currency, created_at, updated_at`, id, name))
}

func (s *Store) SetStatus(ctx context.Context, id pgtype.UUID, status string) (int64, error) {
	tag, err := s.db.Exec(ctx, `UPDATE orgs SET status = $2 WHERE id = $1`, id, status)
	return tag.RowsAffected(), err
}

func (s *Store) SetStatusUnlessPendingDelete(ctx context.Context, id pgtype.UUID, status string) (int64, error) {
	tag, err := s.db.Exec(ctx, `
		UPDATE orgs SET status = $2 WHERE id = $1 AND status <> 'pending_delete'`, id, status)
	return tag.RowsAffected(), err
}

func (s *Store) MarkPendingDelete(ctx context.Context, id pgtype.UUID, onlyActive bool) (int64, error) {
	query := `UPDATE orgs SET status = 'pending_delete' WHERE id = $1 AND status <> 'pending_delete'`
	if onlyActive {
		query = `UPDATE orgs SET status = 'pending_delete' WHERE id = $1 AND status = 'active'`
	}
	tag, err := s.db.Exec(ctx, query, id)
	return tag.RowsAffected(), err
}

func (s *Store) Delete(ctx context.Context, id pgtype.UUID) (int64, error) {
	tag, err := s.db.Exec(ctx, `DELETE FROM orgs WHERE id = $1`, id)
	return tag.RowsAffected(), err
}

// DeletePending removes an organization only while it is still in the
// pending-delete lifecycle state. The predicate must be part of the DELETE:
// callers that first discover purge candidates may race with a restore.
func (s *Store) DeletePending(ctx context.Context, id pgtype.UUID) (int64, error) {
	tag, err := s.db.Exec(ctx, `DELETE FROM orgs WHERE id = $1 AND status = 'pending_delete'`, id)
	return tag.RowsAffected(), err
}

func (s *Store) Status(ctx context.Context, id pgtype.UUID) (string, error) {
	var status string
	err := s.db.QueryRow(ctx, `SELECT status FROM orgs WHERE id = $1`, id).Scan(&status)
	return status, err
}

func (s *Store) Dataplane(ctx context.Context, id pgtype.UUID) (DataplaneSnapshot, error) {
	var out DataplaneSnapshot
	err := s.db.QueryRow(ctx, `SELECT status, currency FROM orgs WHERE id = $1`, id).
		Scan(&out.Status, &out.Currency)
	return out, err
}

func scanOrganization(row pgx.Row) (Organization, error) {
	var out Organization
	err := row.Scan(&out.ID, &out.Slug, &out.Name, &out.Kind, &out.Status,
		&out.Currency, &out.CreatedAt, &out.UpdatedAt)
	return out, err
}
