package usage

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/foundation/cursorpage"
	"github.com/fairlb/fairlb/foundation/db"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
)

// The request-log read model.
//
// It lives here rather than in the console handler for the same reason every
// other read model moved (ADR-0160): the handler's job is DTOs and identity,
// and a query plus its row mapping is neither. What did **not** move is the
// org-scoped transaction the caller opens around this: resolving who may read
// which dimensions is authorization, and it has to happen before the
// transaction starts, because the authorizer uses the same pool and a
// transaction already holding one connection plus an authorizer acquiring a
// second is how a small pool deadlocks against itself.

// ErrLogNotFound is "no such request log entry in this organization".
var ErrLogNotFound = fmt.Errorf("usage: request log not found")

// LogFilter narrows a log listing. A zero-valued optional means "do not filter"
// -- clearing a select in the frontend sends an empty value rather than
// omitting the field, and treating that as a filter for the empty value would
// return nothing at all.
type LogFilter struct {
	From      time.Time
	To        time.Time
	APIKeyID  pgtype.UUID
	Model     string
	Status    string
	EndUserID string
}

// LogEntry is one request as the console reads it.
//
// ChargedNano is present unconditionally; whether the caller may see it is the
// caller's decision, made where the access dimensions are resolved. Zeroing it
// here would mean the read model's shape depended on who asked, and a caller
// that later gained the permission would be reading a different type.
type LogEntry struct {
	// ID is the row id as text: the cursor encodes it, and the query already
	// returns it that way.
	ID          string
	RequestID   string
	CreatedAt   time.Time
	ModelSlug   string
	Surface     string
	Status      string
	HTTPStatus  int32
	ErrorCode   string
	Stream      bool
	TokensIn    int64
	TokensOut   int64
	ChargedNano int64
	DurationMs  int32
	APIKeyID    pgtype.UUID
	EndUserID   string
}

// LogDetail adds what only the single-entry query carries.
type LogDetail struct {
	LogEntry
	ProviderSlug     string
	ChargedCurrency  string
	RouteAttempts    int32
	TTFTMs           int32
	TokensCachedRead int64
	TokensCacheWrite int64
	TokensReasoning  int64
	UsageEstimated   bool
}

// LogReader reads request logs. Bind it to the caller's transaction with
// WithQueries: these reads run under row-level security, so they must be on the
// connection that set the org scope.
type LogReader struct{ q *gwdb.Queries }

func NewLogReader(q *gwdb.Queries) *LogReader { return &LogReader{q: q} }

func entryFrom(r gwdb.ListRequestLogsRow) LogEntry {
	return LogEntry{
		ID: r.ID, RequestID: r.RequestID, CreatedAt: r.CreatedAt.Time,
		ModelSlug: r.ModelSlug, Surface: r.Surface, Status: r.Status,
		HTTPStatus: r.HttpStatus, ErrorCode: r.ErrorCode, Stream: r.Stream,
		TokensIn: int64(r.TokensIn), TokensOut: int64(r.TokensOut),
		ChargedNano: r.ChargedNano, DurationMs: r.DurationMs,
		APIKeyID: r.ApiKeyID, EndUserID: r.EndUserID,
	}
}

// Logs returns one page of request logs, plus whether another page follows.
//
// It fetches one extra row to decide that, which is cheaper than a second COUNT
// query -- on a partitioned table a count has to scan the entire filtered range.
func (r *LogReader) Logs(
	ctx context.Context, orgID pgtype.UUID, f LogFilter, limit int, page cursorpage.Page,
) ([]LogEntry, bool, error) {
	rows, err := r.q.ListRequestLogs(ctx, listParams(orgID, f, page.ProbeLimit(), page))
	if err != nil {
		return nil, false, fmt.Errorf("usage: list request logs: %w", err)
	}
	trimmed, more := cursorpage.Trim(rows, limit)
	out := make([]LogEntry, 0, len(trimmed))
	for _, row := range trimmed {
		out = append(out, entryFrom(row))
	}
	return out, more, nil
}

// LogsForExport returns up to max rows under the same filter the listing uses.
// One filter implementation, so an export can never disagree with the page the
// operator was looking at when they clicked it.
func (r *LogReader) LogsForExport(
	ctx context.Context, orgID pgtype.UUID, f LogFilter, max int32,
) ([]LogEntry, error) {
	rows, err := r.q.ListRequestLogs(ctx, listParams(orgID, f, max, cursorpage.Page{}))
	if err != nil {
		return nil, fmt.Errorf("usage: export request logs: %w", err)
	}
	out := make([]LogEntry, 0, len(rows))
	for _, row := range rows {
		out = append(out, entryFrom(row))
	}
	return out, nil
}

// Log reads one entry, including the routing attempts and cache token buckets.
func (r *LogReader) Log(ctx context.Context, orgID pgtype.UUID, requestID string) (LogDetail, error) {
	row, err := r.q.GetRequestLog(ctx, gwdb.GetRequestLogParams{OrgID: orgID, RequestID: requestID})
	if err != nil {
		if db.IsNoRows(err) {
			return LogDetail{}, ErrLogNotFound
		}
		return LogDetail{}, fmt.Errorf("usage: read request log: %w", err)
	}
	// The shared columns match the list row in name and order, so they map
	// through one function. Two copies of a mapping drift apart sooner or later.
	return LogDetail{
		LogEntry: entryFrom(gwdb.ListRequestLogsRow{
			ID: row.ID, RequestID: row.RequestID, CreatedAt: row.CreatedAt,
			ModelSlug: row.ModelSlug, Surface: row.Surface, Status: row.Status,
			HttpStatus: row.HttpStatus, ErrorCode: row.ErrorCode, Stream: row.Stream,
			TokensIn: row.TokensIn, TokensOut: row.TokensOut,
			ChargedNano: row.ChargedNano, DurationMs: row.DurationMs,
			ApiKeyID: row.ApiKeyID, EndUserID: row.EndUserID,
		}),
		ProviderSlug: row.ProviderSlug, ChargedCurrency: row.ChargedCurrency,
		RouteAttempts: row.RouteAttempts, TTFTMs: row.TtftMs,
		TokensCachedRead: int64(row.TokensCachedRead),
		TokensCacheWrite: int64(row.TokensCacheWrite),
		TokensReasoning:  int64(row.TokensReasoning),
		UsageEstimated:   row.UsageEstimated,
	}, nil
}

func listParams(org pgtype.UUID, f LogFilter, limit int32, page cursorpage.Page) gwdb.ListRequestLogsParams {
	return gwdb.ListRequestLogsParams{
		OrgID:     org,
		FromTs:    pgtype.Timestamptz{Time: f.From, Valid: true},
		ToTs:      pgtype.Timestamptz{Time: f.To, Valid: true},
		ApiKeyID:  f.APIKeyID,
		Model:     textOrNull(f.Model),
		Status:    textOrNull(f.Status),
		EndUserID: textOrNull(f.EndUserID),
		CursorTs:  page.CursorAt,
		CursorID:  page.CursorID,
		Lim:       limit,
	}
}

func textOrNull(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}
