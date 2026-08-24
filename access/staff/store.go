// Package staff owns writes to the public staff identity tables.
package staff

import (
	"context"
	"net/netip"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

type dbtx interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// Store is the concrete write/read contract for staff_users and staff_sessions.
type Store struct{ db dbtx }

func New(pool *pgxpool.Pool) *Store { return &Store{db: pool} }

func (s *Store) WithTx(tx pgx.Tx) *Store { return &Store{db: tx} }

type User struct {
	ID           pgtype.UUID
	Email        string
	PasswordHash string
	Name         string
	Role         string
	Status       string
	CreatedAt    pgtype.Timestamptz
	UpdatedAt    pgtype.Timestamptz
}

type CreateUser struct {
	Email        string
	PasswordHash string
	Name         string
	Role         string
}

func (s *Store) Create(ctx context.Context, in CreateUser) (User, error) {
	return scanUser(s.db.QueryRow(ctx, `
		INSERT INTO staff_users (email, password_hash, name, role)
		VALUES ($1, $2, $3, $4)
		RETURNING id, email, password_hash, name, role, status, created_at, updated_at`,
		in.Email, in.PasswordHash, in.Name, in.Role))
}

// CreateFirst inserts the first staff identity. Callers serialize this method
// with their deployment-claim lock; no row means the deployment is claimed.
func (s *Store) CreateFirst(ctx context.Context, in CreateUser) (User, error) {
	return scanUser(s.db.QueryRow(ctx, `
		INSERT INTO staff_users (email, password_hash, name, role)
		SELECT $1::citext, $2::text, $3::text, $4::text
		WHERE NOT EXISTS (SELECT 1 FROM staff_users)
		RETURNING id, email, password_hash, name, role, status, created_at, updated_at`,
		in.Email, in.PasswordHash, in.Name, in.Role))
}

func scanUser(row pgx.Row) (User, error) {
	var out User
	err := row.Scan(&out.ID, &out.Email, &out.PasswordHash, &out.Name, &out.Role,
		&out.Status, &out.CreatedAt, &out.UpdatedAt)
	return out, err
}

func (s *Store) ByEmail(ctx context.Context, email string) (User, error) {
	return scanUser(s.db.QueryRow(ctx, `
		SELECT id, email, password_hash, name, role, status, created_at, updated_at
		FROM staff_users WHERE email = $1`, email))
}

func (s *Store) ByID(ctx context.Context, id pgtype.UUID) (User, error) {
	return scanUser(s.db.QueryRow(ctx, `
		SELECT id, email, password_hash, name, role, status, created_at, updated_at
		FROM staff_users WHERE id = $1`, id))
}

func (s *Store) UpdatePasswordIfCurrent(ctx context.Context, id pgtype.UUID, current, next string) (int64, error) {
	tag, err := s.db.Exec(ctx, `
		UPDATE staff_users SET password_hash = $3
		WHERE id = $1 AND password_hash = $2`, id, current, next)
	return tag.RowsAffected(), err
}

func (s *Store) SetPassword(ctx context.Context, id pgtype.UUID, passwordHash string) (int64, error) {
	tag, err := s.db.Exec(ctx, `UPDATE staff_users SET password_hash = $2 WHERE id = $1`, id, passwordHash)
	return tag.RowsAffected(), err
}

func (s *Store) SetRole(ctx context.Context, id pgtype.UUID, role string) (User, error) {
	return scanUser(s.db.QueryRow(ctx, `
		UPDATE staff_users SET role = $2 WHERE id = $1
		RETURNING id, email, password_hash, name, role, status, created_at, updated_at`, id, role))
}

func (s *Store) SetStatus(ctx context.Context, id pgtype.UUID, status string) (int64, error) {
	tag, err := s.db.Exec(ctx, `UPDATE staff_users SET status = $2 WHERE id = $1 AND status <> $2`, id, status)
	return tag.RowsAffected(), err
}

type Session struct {
	ID          pgtype.UUID
	StaffUserID pgtype.UUID
	TokenHash   string
	Ip          *netip.Addr
	UserAgent   string
	CreatedAt   pgtype.Timestamptz
	LastSeenAt  pgtype.Timestamptz
	ExpiresAt   pgtype.Timestamptz
	RevokedAt   pgtype.Timestamptz
}

type CreateSession struct {
	StaffUserID pgtype.UUID
	TokenHash   string
	IP          *netip.Addr
	UserAgent   string
	ExpiresAt   pgtype.Timestamptz
}

func (s *Store) CreateSession(ctx context.Context, in CreateSession) (Session, error) {
	return scanSession(s.db.QueryRow(ctx, `
		INSERT INTO staff_sessions (staff_user_id, token_hash, ip, user_agent, expires_at)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, staff_user_id, token_hash, ip, user_agent, created_at, last_seen_at, expires_at, revoked_at`,
		in.StaffUserID, in.TokenHash, in.IP, in.UserAgent, in.ExpiresAt))
}

func scanSession(row pgx.Row) (Session, error) {
	var out Session
	err := row.Scan(&out.ID, &out.StaffUserID, &out.TokenHash, &out.Ip, &out.UserAgent,
		&out.CreatedAt, &out.LastSeenAt, &out.ExpiresAt, &out.RevokedAt)
	return out, err
}

type AuthSession struct {
	ID          pgtype.UUID
	StaffUserID pgtype.UUID
	LastSeenAt  pgtype.Timestamptz
	ExpiresAt   pgtype.Timestamptz
	StaffStatus string
	StaffRole   string
}

func (s *Store) SessionForAuth(ctx context.Context, tokenHash string) (AuthSession, error) {
	var out AuthSession
	err := s.db.QueryRow(ctx, `
		SELECT s.id, s.staff_user_id, s.last_seen_at, s.expires_at, u.status, u.role
		FROM staff_sessions s JOIN staff_users u ON u.id = s.staff_user_id
		WHERE s.token_hash = $1 AND s.revoked_at IS NULL AND s.expires_at > now()`, tokenHash).
		Scan(&out.ID, &out.StaffUserID, &out.LastSeenAt, &out.ExpiresAt, &out.StaffStatus, &out.StaffRole)
	return out, err
}

func (s *Store) TouchSession(ctx context.Context, id pgtype.UUID, expiresAt pgtype.Timestamptz) error {
	_, err := s.db.Exec(ctx, `
		UPDATE staff_sessions SET last_seen_at = now(), expires_at = $2
		WHERE id = $1 AND revoked_at IS NULL`, id, expiresAt)
	return err
}

func (s *Store) RevokeSessionByTokenHash(ctx context.Context, tokenHash string) error {
	_, err := s.db.Exec(ctx, `UPDATE staff_sessions SET revoked_at = now() WHERE token_hash = $1 AND revoked_at IS NULL`, tokenHash)
	return err
}

func (s *Store) RevokeSession(ctx context.Context, staffID, sessionID pgtype.UUID) (int64, error) {
	tag, err := s.db.Exec(ctx, `
		UPDATE staff_sessions SET revoked_at = now()
		WHERE id = $1 AND staff_user_id = $2 AND revoked_at IS NULL`, sessionID, staffID)
	return tag.RowsAffected(), err
}

func (s *Store) RevokeOtherSessions(ctx context.Context, staffID, keepID pgtype.UUID) error {
	_, err := s.db.Exec(ctx, `
		UPDATE staff_sessions SET revoked_at = now()
		WHERE staff_user_id = $1 AND id <> $2 AND revoked_at IS NULL`, staffID, keepID)
	return err
}

func (s *Store) RevokeSessions(ctx context.Context, staffID pgtype.UUID) error {
	_, err := s.db.Exec(ctx, `UPDATE staff_sessions SET revoked_at = now() WHERE staff_user_id = $1 AND revoked_at IS NULL`, staffID)
	return err
}

func (s *Store) DeleteExpiredSessions(ctx context.Context) (int64, error) {
	tag, err := s.db.Exec(ctx, `
		DELETE FROM staff_sessions
		WHERE expires_at < now() - interval '30 days'
		   OR (revoked_at IS NOT NULL AND revoked_at < now() - interval '30 days')`)
	return tag.RowsAffected(), err
}

func (s *Store) ListActiveSessions(
	ctx context.Context,
	staffID pgtype.UUID,
	limit int32,
	cursorCreatedAt pgtype.Timestamptz,
	cursorID pgtype.UUID,
) ([]Session, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, staff_user_id, token_hash, ip, user_agent, created_at, last_seen_at, expires_at, revoked_at
		FROM staff_sessions
		WHERE staff_user_id = $1 AND revoked_at IS NULL AND expires_at > now()
		  AND ($3::timestamptz IS NULL OR (created_at, id) < ($3::timestamptz, $4::uuid))
		ORDER BY created_at DESC, id DESC LIMIT $2`, staffID, limit, cursorCreatedAt, cursorID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Session, 0)
	for rows.Next() {
		row, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
