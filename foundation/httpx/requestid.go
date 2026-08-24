// Package httpx is the HTTP middleware stack shared by every API plane:
// request id, authentication domain, audit hook, idempotency, rate limiting,
// problem+json rendering and graceful shutdown.
package httpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota
	ctxKeyPrincipal
	ctxKeyAuditState
)

// HeaderRequestID is the correlation header carried by every response.
const HeaderRequestID = "X-Request-Id"

// RequestID mints a server-side id for each request — inbound headers are never
// trusted for this — and puts it in the context and the response headers.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := NewRequestID()
		w.Header().Set(HeaderRequestID, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyRequestID, id)))
	})
}

// RequestIDFrom returns the current request id, or the empty string.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(ctxKeyRequestID).(string)
	return id
}

// NewRequestID mints the correlation key shared by HTTP headers, usage rows
// and settlement records.
func NewRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "req_" + hex.EncodeToString(b[:])
}
