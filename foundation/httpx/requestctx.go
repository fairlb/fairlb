package httpx

import (
	"context"
	"net/http"
)

// The raw request travels in the context for the strict-server handlers that
// record who did something from where (audit rows, session device rows). Each
// generated API package keeps its own one-line strict middleware, because the
// StrictHandlerFunc type is generated per package; the key, the accessor and the
// two derived readings live here so that there is one of each (ADR-0206 -- three
// packages used to carry an identical private copy).

type ctxKeyRequest struct{}

// WithRequest stores r in ctx for RequestFrom and its derived readers.
func WithRequest(ctx context.Context, r *http.Request) context.Context {
	return context.WithValue(ctx, ctxKeyRequest{}, r)
}

// RequestFrom returns the request stored by WithRequest, or nil outside a
// handler assembled with it.
func RequestFrom(ctx context.Context) *http.Request {
	r, _ := ctx.Value(ctxKeyRequest{}).(*http.Request)
	return r
}

// ClientIPFrom is ClientIP over the stored request; "" when there is none.
func ClientIPFrom(ctx context.Context) string {
	if r := RequestFrom(ctx); r != nil {
		return ClientIP(r)
	}
	return ""
}

// UserAgentFrom is the stored request's User-Agent; "" when there is none.
func UserAgentFrom(ctx context.Context) string {
	if r := RequestFrom(ctx); r != nil {
		return r.UserAgent()
	}
	return ""
}
