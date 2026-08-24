// Package audit records what was done and by whom.
//
// There are two layers of writes:
//   - the middleware fallback row: one HTTP-level who/what/when per write
//     request, including one that panicked, stored by the Hook from a deferred
//     call;
//   - detailed rows written by a domain: high-value actions (credit
//     adjustments, suspensions, impersonation, settings changes) write a row
//     with a target and metadata through InsertTx, inside the business
//     transaction, so it rolls back with the change it describes.
//
// Deduplication: after committing, the domain calls httpx.MarkAudited, and the
// Hook then skips the fallback row. If the transaction rolls back the mark is
// never set and the fallback row still records the failed attempt.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/fairlb/fairlb/foundation/httpx"
	"net/netip"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

type dbtx interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// Entry is what a domain supplies for one audit record. A zero-value ActorID or
// OrgID is stored as NULL.
type Entry struct {
	ActorType  string // user | staff | system
	ActorID    pgtype.UUID
	OrgID      pgtype.UUID
	Action     string
	TargetType string
	TargetID   string
	Meta       any // serialized to jsonb; nil becomes {}
	IP         string
	RequestID  string
}

// InsertTx writes one audit row on the caller's transaction. The correct
// sequence is InsertTx inside the business transaction, then httpx.MarkAudited
// *after* that transaction commits.
//
// It takes the transaction rather than a query handle bound to it. The two are
// equivalent for a caller whose own queries live in this package's layer, but
// not for one whose queries live in another: that caller would otherwise have
// to carry a second handle from this layer for no reason other than writing an
// audit row. Taking the transaction also makes "already bound to it" a property
// of the signature instead of a sentence in this comment.
func InsertTx(ctx context.Context, db dbtx, e Entry) error {
	meta, err := marshalMeta(e.Meta)
	if err != nil {
		return fmt.Errorf("audit: marshal meta: %w", err)
	}
	_, err = db.Exec(ctx, `
		INSERT INTO audit_logs
		(actor_type, actor_id, org_id, action, target_type, target_id, meta, ip, request_id)
		VALUES ($1, NULLIF($2, '00000000-0000-0000-0000-000000000000'::uuid),
		        NULLIF($3, '00000000-0000-0000-0000-000000000000'::uuid),
		        $4, $5, $6, $7, $8, $9)`,
		e.ActorType, e.ActorID, e.OrgID, e.Action, e.TargetType, e.TargetID,
		meta, httpx.ParseAddr(e.IP), e.RequestID)
	return err
}

type Log struct {
	ID         pgtype.UUID
	ActorType  string
	ActorID    pgtype.UUID
	OrgID      pgtype.UUID
	Action     string
	TargetType string
	TargetID   string
	Meta       []byte
	Ip         *netip.Addr
	RequestID  string
	CreatedAt  pgtype.Timestamptz
}

type Filter struct {
	Limit     int32
	ActorType pgtype.Text
	ActorID   pgtype.UUID
	OrgID     pgtype.UUID
	Action    pgtype.Text
	From      pgtype.Timestamptz
	To        pgtype.Timestamptz
	CursorAt  pgtype.Timestamptz
	CursorID  pgtype.UUID
}

type Store struct{ db dbtx }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{db: pool} }

func (s *Store) WithTx(tx pgx.Tx) *Store { return &Store{db: tx} }

func (s *Store) List(ctx context.Context, f Filter) ([]Log, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, actor_type, actor_id, org_id, action, target_type, target_id,
		       meta, ip, request_id, created_at
		FROM audit_logs
		WHERE ($2::text IS NULL OR actor_type = $2)
		  AND ($3::uuid IS NULL OR actor_id = $3)
		  AND ($4::uuid IS NULL OR org_id = $4)
		  AND ($5::text IS NULL OR action = $5)
		  AND ($6::timestamptz IS NULL OR created_at >= $6)
		  AND ($7::timestamptz IS NULL OR created_at < $7)
		  AND ($8::timestamptz IS NULL OR created_at < $8 OR (created_at = $8 AND id < $9::uuid))
		ORDER BY created_at DESC, id DESC LIMIT $1`,
		f.Limit, f.ActorType, f.ActorID, f.OrgID, f.Action, f.From, f.To, f.CursorAt, f.CursorID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Log, 0)
	for rows.Next() {
		var row Log
		if err := rows.Scan(&row.ID, &row.ActorType, &row.ActorID, &row.OrgID,
			&row.Action, &row.TargetType, &row.TargetID, &row.Meta, &row.Ip,
			&row.RequestID, &row.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func marshalMeta(m any) ([]byte, error) {
	if m == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(m)
}
