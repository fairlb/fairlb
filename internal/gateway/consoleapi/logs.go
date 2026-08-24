package gwconsoleapi

import (
	"context"
	"encoding/csv"
	"github.com/fairlb/fairlb/foundation/strutil"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"errors"
	"github.com/fairlb/fairlb/foundation/cursorpage"
	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/foundation/money"
	"github.com/fairlb/fairlb/foundation/publicid"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
	"github.com/fairlb/fairlb/internal/gateway/usage"
)

// Per-request logs. Aggregation lives with the usage endpoints; this file only
// filters and paginates individual requests.

const (
	defaultLogLimit = 50
	maxLogLimit     = 200
	// defaultLogWindow is the window used when no time parameters are given.
	// The most common thing anyone does on this page is ask "what happened to
	// the request I just made", not browse history.
	defaultLogWindow = 24 * time.Hour
	// maxCSVLogRows caps the export. Per-request rows have none of the natural
	// compression the rollups have, and a month's worth is enough for one
	// export to tie up both memory and a connection.
	maxCSVLogRows = 50_000
)

// logFilter is the filter the three endpoints share.
// buildFilter normalises the filter parameters into the read model's own.
//
// An empty string means "do not filter": clearing a select in the frontend
// sends an empty string rather than omitting the field, and treating that as a
// filter for the empty value would return nothing at all.
func buildFilter(from, to *time.Time, apiKeyID, model, status, endUser *string) (usage.LogFilter, error) {
	f := usage.LogFilter{To: time.Now()}
	if to != nil {
		f.To = *to
	}
	f.From = f.To.Add(-defaultLogWindow)
	if from != nil {
		f.From = *from
	}
	var err error
	if f.From, f.To, err = validRange(f.From, f.To); err != nil {
		return usage.LogFilter{}, err
	}
	if s := strutil.Deref(apiKeyID); s != "" {
		id, pErr := publicid.Parse(publicid.Key, s)
		if pErr != nil {
			return usage.LogFilter{}, httpx.ErrCodeDetail(errcode.CommonValidation, "Invalid api_key_id")
		}
		f.APIKeyID = id
	}
	f.Model, f.Status, f.EndUserID = strutil.Deref(model), strutil.Deref(status), strutil.Deref(endUser)
	return f, nil
}

// ListRequestLogs returns one page of request logs.
func (s *Server) ListRequestLogs(ctx context.Context, req ListRequestLogsRequestObject) (ListRequestLogsResponseObject, error) {
	p := req.Params
	f, err := buildFilter(p.From, p.To, p.ApiKeyId, p.Model, (*string)(p.Status), p.EndUserId)
	if err != nil {
		return nil, err
	}
	page, err := httpx.ParseCursorPage(p.Cursor, p.Limit, defaultLogLimit, maxLogLimit)
	if err != nil {
		return nil, err
	}

	var entries []usage.LogEntry
	var more bool
	access := orgReadAccess{}
	err = s.scopeRead(ctx, req.OrgId, orgReadRequirements{KeyMetadata: f.APIKeyID.Valid}, func(ctx context.Context, q *gwdb.Queries, org pgtype.UUID, resolved orgReadAccess) error {
		access = resolved
		var qErr error
		entries, more, qErr = usage.NewLogReader(q).Logs(ctx, org, f, int(page.Limit), page)
		return qErr
	})
	if err != nil {
		return nil, err
	}

	var next *string
	if more && len(entries) > 0 {
		last := entries[len(entries)-1]
		c := cursorpage.Encode(last.CreatedAt, last.ID)
		next = &c
	}
	items := make([]RequestLog, 0, len(entries))
	for _, e := range entries {
		items = append(items, logDTO(e, access))
	}
	return ListRequestLogs200JSONResponse{Items: items, NextCursor: next}, nil
}

// GetRequestLog returns one log entry, including the chain of routing attempts
// and the cache token buckets.
func (s *Server) GetRequestLog(ctx context.Context, req GetRequestLogRequestObject) (GetRequestLogResponseObject, error) {
	var detail usage.LogDetail
	access := orgReadAccess{}
	err := s.scopeRead(ctx, req.OrgId, orgReadRequirements{}, func(ctx context.Context, q *gwdb.Queries, org pgtype.UUID, resolved orgReadAccess) error {
		access = resolved
		var qErr error
		detail, qErr = usage.NewLogReader(q).Log(ctx, org, req.RequestId)
		if errors.Is(qErr, usage.ErrLogNotFound) {
			return httpx.ErrCodeDetail(errcode.CommonNotFound, "No such request log entry")
		}
		return qErr
	})
	if err != nil {
		return nil, err
	}

	// The generator flattens allOf into a single struct rather than embedding,
	// so the shared columns are mapped by logDTO first and then copied across
	// field by field. Both mappings come from one source and cannot drift.
	base := logDTO(detail.LogEntry, access)
	d := RequestLogDetail{
		RequestId: base.RequestId, CreatedAt: base.CreatedAt, ModelSlug: base.ModelSlug,
		Surface: base.Surface, Status: base.Status, HttpStatus: base.HttpStatus,
		ErrorCode: base.ErrorCode, Stream: base.Stream, TokensIn: base.TokensIn,
		TokensOut: base.TokensOut, ChargedNano: base.ChargedNano,
		DurationMs: base.DurationMs, ApiKeyId: base.ApiKeyId, EndUserId: base.EndUserId,

		RouteAttempts:    ptr(int(detail.RouteAttempts)),
		TtftMs:           ptr(int(detail.TTFTMs)),
		TokensCachedRead: ptr(detail.TokensCachedRead),
		TokensCacheWrite: ptr(detail.TokensCacheWrite),
		TokensReasoning:  ptr(detail.TokensReasoning),
		UsageEstimated:   &detail.UsageEstimated,
	}
	if access.Finance {
		currency := detail.ChargedCurrency
		d.ChargedCurrency = &currency
	}
	if detail.ProviderSlug != "" {
		slug := detail.ProviderSlug
		d.ProviderSlug = &slug
	}
	return GetRequestLog200JSONResponse(d), nil
}

// ExportLogsCSV exports logs under exactly the filters the list uses.
func (s *Server) ExportLogsCSV(ctx context.Context, req ExportLogsCSVRequestObject) (ExportLogsCSVResponseObject, error) {
	p := req.Params
	f, err := buildFilter(p.From, p.To, p.ApiKeyId, p.Model, (*string)(p.Status), p.EndUserId)
	if err != nil {
		return nil, err
	}
	var entries []usage.LogEntry
	require := orgReadRequirements{Finance: true, KeyMetadata: true}
	err = s.scopeRead(ctx, req.OrgId, require, func(ctx context.Context, q *gwdb.Queries, org pgtype.UUID, _ orgReadAccess) error {
		var qErr error
		entries, qErr = usage.NewLogReader(q).LogsForExport(ctx, org, f, maxCSVLogRows)
		return qErr
	})
	if err != nil {
		return nil, err
	}
	return logsCSVResponse{rows: entries}, nil
}

type logsCSVResponse struct{ rows []usage.LogEntry }

func (r logsCSVResponse) VisitExportLogsCSVResponse(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="logs.csv"`)
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{
		"created_at", "request_id", "model", "surface", "status", "http_status",
		"error_code", "stream", "tokens_in", "tokens_out", "charged", "charged_nano",
		"duration_ms", "api_key_id", "end_user_id",
	}); err != nil {
		return err
	}
	for _, l := range r.rows {
		keyID := ""
		if l.APIKeyID.Valid {
			keyID = publicid.Format(publicid.Key, l.APIKeyID)
		}
		if err := cw.Write([]string{
			l.CreatedAt.UTC().Format(time.RFC3339),
			l.RequestID, l.ModelSlug, l.Surface, l.Status,
			strconv.Itoa(int(l.HTTPStatus)), l.ErrorCode, strconv.FormatBool(l.Stream),
			strconv.FormatInt(l.TokensIn, 10), strconv.FormatInt(l.TokensOut, 10),
			money.FormatNano(l.ChargedNano), strconv.FormatInt(l.ChargedNano, 10),
			strconv.Itoa(int(l.DurationMs)), keyID, l.EndUserID,
		}); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// logDTO renders one entry.
//
// The finance dimension is applied here, not in the read model: whether a
// caller may see what a request cost is a property of the caller, and a read
// model whose shape depended on that would be a different type for each
// audience.
func logDTO(e usage.LogEntry, access orgReadAccess) RequestLog {
	chargedNano := e.ChargedNano
	if !access.Finance {
		chargedNano = 0
	}
	l := RequestLog{
		RequestId: e.RequestID, CreatedAt: e.CreatedAt, ModelSlug: e.ModelSlug,
		Surface: e.Surface, Status: e.Status, HttpStatus: int(e.HTTPStatus),
		TokensIn: ptr(e.TokensIn), TokensOut: ptr(e.TokensOut),
		ChargedNano: chargedNano, DurationMs: ptr(int(e.DurationMs)),
		Stream: &e.Stream,
	}
	if e.ErrorCode != "" {
		code := e.ErrorCode
		l.ErrorCode = &code
	}
	if e.EndUserID != "" {
		id := e.EndUserID
		l.EndUserId = &id
	}
	if access.KeyMetadata && e.APIKeyID.Valid {
		l.ApiKeyId = ptr(publicid.Format(publicid.Key, e.APIKeyID))
	}
	return l
}

func ptr[T any](v T) *T { return &v }
