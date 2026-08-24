package audit

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fairlb/fairlb/foundation/db"
	"github.com/fairlb/fairlb/foundation/httpx"
)

// hookWriteTimeout budgets the fallback row's write. It happens after the
// response and is best effort; it must not hold up finishing the request.
const hookWriteTimeout = 3 * time.Second

// Hook is the database-backed implementation of httpx.AuditHook. It consults
// WasAudited: if a domain already wrote a detailed row it does nothing,
// otherwise it writes the HTTP-level fallback row.
type Hook struct {
	pool *pgxpool.Pool
}

func NewHook(pool *pgxpool.Pool) *Hook { return &Hook{pool: pool} }

// Record writes the fallback audit row. It detaches from the request lifetime
// (WithoutCancel plus its own timeout) so a client hanging up does not lose the
// record. A failure is logged and never fails the request: a wobble in the audit
// store must not take the plane down with it.
func (h *Hook) Record(ctx context.Context, ev httpx.AuditEvent) {
	if httpx.WasAudited(ctx) {
		return // a domain already wrote its own row inside the business transaction
	}
	actorType, actorID := mapActor(ev.Principal)
	e := Entry{
		ActorType: actorType,
		ActorID:   actorID,
		Action:    ev.Method + " " + ev.Path,
		Meta: map[string]any{
			"status":      ev.Status,
			"duration_ms": ev.DurationMS,
		},
		IP:        ev.RemoteIP,
		RequestID: ev.RequestID,
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), hookWriteTimeout)
	defer cancel()
	if err := db.WithSystemTx(wctx, h.pool, func(tx pgx.Tx) error {
		return InsertTx(wctx, tx, e)
	}); err != nil {
		slog.ErrorContext(ctx, "failed to store fallback audit row", "error", err, "request_id", ev.RequestID)
	}
}

var _ httpx.AuditHook = (*Hook)(nil)

// mapActor turns an authenticated subject into an audit actor. An anonymous
// subject — a sign-in attempt, say — has no actor id.
func mapActor(p httpx.Principal) (string, pgtype.UUID) {
	var id pgtype.UUID
	if p.Subject != "" {
		_ = id.Scan(p.Subject)
	}
	switch p.Scope {
	case "admin":
		return "staff", id
	case "console":
		return "user", id
	default:
		return "system", id
	}
}
