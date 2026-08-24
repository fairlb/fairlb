package httpx

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"

	"github.com/fairlb/fairlb/foundation/errcode"
)

// Problem is an RFC 9457 error body plus a stable machine-readable code.
// The data plane is the exception: it renders upstream-compatible error shapes
// of its own so that existing SDKs keep working.
type Problem struct {
	Type      string `json:"type,omitempty"`
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Detail    string `json:"detail,omitempty"`
	Instance  string `json:"instance,omitempty"`
	Code      string `json:"code"`
	RequestID string `json:"request_id,omitempty"`
}

// Error renders an error response from the registry; code must be defined in a
// segment of the error code registry this build ships, of which
// api/errors-core.yaml is the one every build has. The lookup takes a bare code
// string on purpose: it sits below the layers whose codes it renders and cannot
// name them.
func Error(w http.ResponseWriter, r *http.Request, code, detail string) {
	def, ok := errcode.Lookup(code)
	if !ok {
		// An unregistered code is a programming error: fall back to the
		// internal-error definition and leave a trace.
		slog.ErrorContext(r.Context(), "unregistered error code", "code", code)
		def, _ = errcode.Lookup(errcode.CommonInternal)
	}
	writeProblem(w, r, Problem{
		Title:  def.Title,
		Status: def.Status,
		Detail: detail,
		Code:   def.Code,
	})
}

func writeProblem(w http.ResponseWriter, r *http.Request, p Problem) {
	p.RequestID = RequestIDFrom(r.Context())
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}

// Recover turns a panic into a 500 problem document and logs it. The stack goes
// to the log, never into the response.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler { //nolint:errorlint // a sentinel value is compared by identity, by convention
					panic(rec)
				}
				slog.ErrorContext(r.Context(), "panic", "err", rec, "stack", string(debug.Stack()))
				Error(w, r, errcode.CommonInternal, "")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// NotFoundHandler and MethodNotAllowedHandler are mounted on the router so that
// 404 and 405 are problem+json too, rather than the router's plain text.
func NotFoundHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		Error(w, r, errcode.CommonNotFound, "")
	}
}

func MethodNotAllowedHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		Error(w, r, errcode.CommonMethodNotAllowed, "")
	}
}

// OAPIRequestError renders binding and validation failures from the generated
// server, replacing its default plain-text http.Error output. The detail stays
// generic for the client; the underlying error goes only to the structured log,
// where the request id correlates the two.
func OAPIRequestError(w http.ResponseWriter, r *http.Request, err error) {
	slog.WarnContext(r.Context(), "request binding failed", "error", err)
	Error(w, r, errcode.CommonValidation, "The request is not valid.")
}

// OAPIResponseError renders errors returned by handlers: a CodeError is
// rendered from the registry (the business-error exit for a handler), anything
// else falls back to 500.
func OAPIResponseError(w http.ResponseWriter, r *http.Request, err error) {
	var ce *CodeError
	if errors.As(err, &ce) {
		if ce.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(ce.RetryAfter))
		}
		Error(w, r, ce.Code, ce.Detail)
		return
	}
	slog.ErrorContext(r.Context(), "handler error", "error", err)
	Error(w, r, errcode.CommonInternal, "")
}
