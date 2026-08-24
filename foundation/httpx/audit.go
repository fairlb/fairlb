package httpx

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// AuditEvent is the minimal event surface handed to an audit hook.
type AuditEvent struct {
	Time       time.Time
	RequestID  string
	Principal  Principal
	Method     string
	Path       string
	Status     int
	DurationMS int64
	RemoteIP   string
}

// AuditHook is the audit extension point; an implementation must not block the
// request path for long. The context carries the dedup state (see WasAudited)
// and tracing values. An implementation that writes to a database must detach
// from the request lifetime (context.WithoutCancel): a client hanging up must
// not lose the audit record.
type AuditHook interface {
	Record(ctx context.Context, ev AuditEvent)
}

// SlogAuditHook writes events to the structured log. It is the default; the hook
// that persists events to the database lives in the audit package.
type SlogAuditHook struct{}

func (SlogAuditHook) Record(_ context.Context, ev AuditEvent) {
	slog.Info("audit",
		"request_id", ev.RequestID,
		"scope", ev.Principal.Scope,
		"subject", ev.Principal.Subject,
		"method", ev.Method,
		"path", ev.Path,
		"status", ev.Status,
		"duration_ms", ev.DurationMS,
		"ip", ev.RemoteIP,
	)
}

// auditState is the per-request dedup flag. A domain that wrote its own
// detailed audit row sets it, which suppresses the middleware's generic
// fallback row. The pointer is stored in the context so every derived context
// shares one state.
type auditState struct{ recorded bool }

// MarkAudited is called by a domain that wrote a detailed audit row, and must be
// called *after* the transaction commits. Only a row that actually landed may
// suppress the fallback: if the transaction rolls back, the flag stays unset and
// the middleware still records the failed attempt.
func MarkAudited(ctx context.Context) {
	if st, _ := ctx.Value(ctxKeyAuditState).(*auditState); st != nil {
		st.recorded = true
	}
}

// WasAudited reports whether this request already wrote a detailed audit row;
// the database hook uses it to skip the fallback row.
func WasAudited(ctx context.Context) bool {
	st, _ := ctx.Value(ctxKeyAuditState).(*auditState)
	return st != nil && st.recorded
}

// Audit calls the hook after a write operation (anything but GET/HEAD/OPTIONS)
// finishes. It records from a deferred function so that a panicking handler is
// audited too (status 500) before the panic is re-raised for the recovery
// middleware to render and log. http.ErrAbortHandler passes through unrecorded:
// by convention that sentinel means the client hung up, not that anything
// happened worth auditing.
func Audit(hook AuditHook) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
				next.ServeHTTP(w, r)
				return
			}
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w}
			ctx := context.WithValue(r.Context(), ctxKeyAuditState, &auditState{})
			r = r.WithContext(ctx)
			defer func() {
				rec := recover()
				if rec == http.ErrAbortHandler { //nolint:errorlint // a sentinel value is compared by identity, by convention
					panic(rec)
				}
				status := sw.status()
				if rec != nil {
					status = http.StatusInternalServerError
				}
				hook.Record(ctx, AuditEvent{
					Time:       start,
					RequestID:  RequestIDFrom(ctx),
					Principal:  PrincipalFrom(ctx),
					Method:     r.Method,
					Path:       r.URL.Path,
					Status:     status,
					DurationMS: time.Since(start).Milliseconds(),
					RemoteIP:   r.RemoteAddr,
				})
				if rec != nil {
					panic(rec)
				}
			}()
			next.ServeHTTP(sw, r)
		})
	}
}

// statusWriter records the status code that was written (200 by default).
type statusWriter struct {
	http.ResponseWriter
	code int
}

// Unwrap lets http.ResponseController reach the underlying writer through this
// wrapper. Without it a wrapped handler loses flushing and deadline control, and
// a streaming response is buffered until the handler returns — which is the same
// thing as not streaming at all.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) status() int {
	if w.code == 0 {
		return http.StatusOK
	}
	return w.code
}
