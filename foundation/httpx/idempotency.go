package httpx

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fairlb/fairlb/foundation/db"
	"github.com/fairlb/fairlb/foundation/errcode"
)

// HeaderIdempotencyKey lets money-moving and resource-creating POSTs be retried
// safely. The contract: first sighting executes and stores; a repeat replays;
// the same key with a different request is 422; a first attempt still running
// is 409. ADR-0172 made the header visible on the console operations that
// carry it.
const HeaderIdempotencyKey = "Idempotency-Key"

const (
	idempotencyTTL = 24 * time.Hour
	// inFlightTimeout: a first attempt that has been silent for longer than
	// this is presumed dead (crashed process, killed container) and a retry
	// with the same fingerprint may take it over. Without a takeover, a crash
	// would pin the key until the TTL expires and every retry would get 409 for
	// a full day.
	inFlightTimeout = 60 * time.Second
	// maxCapturedBody bounds both the request body we can hash and the response
	// body we can store for replay.
	maxCapturedBody = 1 << 20
	maxKeyLength    = 255
)

// Idempotency stores the first result for an Idempotency-Key and replays it:
// first sighting executes and stores; a repeat of the same key replays the
// stored response; the same key with a different request is 422; a first attempt
// still running is 409; a first attempt that has gone silent is taken over by
// the retry. Only POSTs that carry the header are affected. The key is scoped to
// the authenticated subject (or, for anonymous callers, the client address), so
// two callers cannot collide on the same string. A 5xx or 429 outcome is not
// stored: the retry the header exists for must be able to execute.
func Idempotency(pool *pgxpool.Pool) func(http.Handler) http.Handler {
	store := newIdempotencyStore(pool)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get(HeaderIdempotencyKey)
			if r.Method != http.MethodPost || key == "" {
				next.ServeHTTP(w, r)
				return
			}
			if len(key) > maxKeyLength {
				Error(w, r, errcode.CommonValidation, "Idempotency-Key is too long.")
				return
			}

			body, err := io.ReadAll(io.LimitReader(r.Body, maxCapturedBody+1))
			if err != nil {
				// This is where the plane-level BodyLimit surfaces.
				var mbe *http.MaxBytesError
				if errors.As(err, &mbe) {
					Error(w, r, errcode.CommonPayloadTooLarge, "")
					return
				}
				Error(w, r, errcode.CommonValidation, "The request body could not be read.")
				return
			}
			if len(body) > maxCapturedBody {
				// Oversize must be rejected explicitly: passing a truncated
				// body to the handler silently corrupts the request.
				Error(w, r, errcode.CommonPayloadTooLarge, "A request carrying an Idempotency-Key is limited to 1 MiB.")
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))

			p := PrincipalFrom(r.Context())
			scope := p.Scope
			if p.Subject != "" {
				scope += ":" + p.Subject
			} else {
				// Anonymous callers (login, register, redeem a signup code) have
				// no subject to scope by. Without one they would share a single
				// keyspace, and anyone could park a key string that then
				// answers 422 — or replays their response — to the next
				// stranger using it. The client address is the narrowest
				// identity available; a NAT shares it, a stranger does not.
				scope += ":ip:" + ClientIP(r)
			}
			hash := requestHash(r, body)
			ctx := r.Context()

			expiresAt := pgtype.Timestamptz{Time: time.Now().Add(idempotencyTTL), Valid: true}
			if err := store.claim(ctx, scope, key, hash, expiresAt); err == nil {
				executeAndStore(w, r, next, store, scope, key, hash)
				return
			} else if !db.IsNoRows(err) {
				slog.ErrorContext(ctx, "failed to claim idempotency key", "error", err)
				Error(w, r, errcode.CommonInternal, "")
				return
			}

			existing, err := store.get(ctx, scope, key)
			if db.IsNoRows(err) {
				// The claim collided, but the row was then removed by the
				// expiry sweep. Retry the claim once.
				if err := store.claim(ctx, scope, key, hash, expiresAt); err == nil {
					executeAndStore(w, r, next, store, scope, key, hash)
					return
				}
				Error(w, r, errcode.CommonInternal, "")
				return
			}
			if err != nil {
				slog.ErrorContext(ctx, "failed to read idempotency key", "error", err)
				Error(w, r, errcode.CommonInternal, "")
				return
			}

			if existing.RequestHash != hash {
				Error(w, r, errcode.CommonIdempotencyMismatch, "")
				return
			}
			if existing.Status == "completed" {
				writeReplay(w, existing)
				return
			}

			// in_flight: try to take over a first attempt that has gone silent.
			err = store.takeOver(ctx, scope, key, hash,
				pgtype.Timestamptz{Time: time.Now().Add(idempotencyTTL), Valid: true},
				pgtype.Timestamptz{Time: time.Now().Add(-inFlightTimeout), Valid: true})
			switch {
			case err == nil:
				executeAndStore(w, r, next, store, scope, key, hash)
			case db.IsNoRows(err):
				Error(w, r, errcode.CommonIdempotencyInFlight, "")
			default:
				slog.ErrorContext(ctx, "failed to take over idempotency key", "error", err)
				Error(w, r, errcode.CommonInternal, "")
			}
		})
	}
}

// executeAndStore runs the handler as the executing attempt and stores the
// response. The completing write is guarded on status and fingerprint: if this
// attempt was taken over while it ran, its result is discarded. Both callers
// still receive their own response; only the first one to complete is stored.
func executeAndStore(w http.ResponseWriter, r *http.Request, next http.Handler, store *idempotencyStore, scope, key, hash string) {
	ctx := r.Context()
	rec := &captureWriter{ResponseWriter: w, buf: &bytes.Buffer{}}
	next.ServeHTTP(rec, r)

	if rec.overflow {
		// An oversize response gets no idempotency guarantee: vacate the slot
		// so a retry executes again rather than replaying a truncated body.
		if err := store.vacate(ctx, scope, key, hash); err != nil {
			slog.ErrorContext(ctx, "failed to vacate idempotency key", "error", err)
		}
		slog.WarnContext(ctx, "response exceeds the idempotency storage limit; not stored", "bytes", rec.buf.Len())
		return
	}
	if status := rec.status(); status >= http.StatusInternalServerError || status == http.StatusTooManyRequests {
		// A failure on our side is not an outcome worth replaying: storing it
		// would answer every retry with the same 500 for a day, and the
		// money-moving endpoints this header exists for are exactly the ones a
		// well-behaved client retries with the same key. Vacate so the retry
		// executes; a 4xx is the caller's own outcome and is kept.
		if err := store.vacate(ctx, scope, key, hash); err != nil {
			slog.ErrorContext(ctx, "failed to vacate idempotency key after a failed attempt", "error", err)
		}
		return
	}
	headers, _ := json.Marshal(storableHeaders(rec.Header()))
	err := store.complete(ctx, scope, key, hash,
		pgtype.Int4{Int32: int32(rec.status()), Valid: true}, //nolint:gosec // an HTTP status code always fits in an int32
		headers, rec.buf.Bytes())
	if err != nil {
		slog.ErrorContext(ctx, "failed to complete idempotency key", "error", err)
	}
}

// replayHeaderBlocklist lists response headers that must not be stored for
// replay. Replaying Set-Cookie would resurrect an expired session credential;
// tracing and rate-limit headers would be stale values describing the *first*
// request rather than this one.
var replayHeaderBlocklist = map[string]struct{}{
	"Set-Cookie":            {},
	HeaderRequestID:         {},
	"X-Ratelimit-Limit":     {},
	"X-Ratelimit-Remaining": {},
	"X-Ratelimit-Reset":     {},
}

// storableHeaders returns the response headers that may be stored for replay.
func storableHeaders(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, vs := range h {
		if _, blocked := replayHeaderBlocklist[http.CanonicalHeaderKey(k)]; blocked {
			continue
		}
		out[k] = vs
	}
	return out
}

// writeReplay replays the stored response of a completed first attempt. The
// headers were already filtered on the way in.
func writeReplay(w http.ResponseWriter, existing idempotencyRow) {
	var headers map[string][]string
	_ = json.Unmarshal(existing.ResponseHeaders, &headers)
	for k, vs := range headers {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("Idempotency-Replayed", "true")
	w.WriteHeader(int(existing.ResponseStatus.Int32))
	_, _ = w.Write(existing.ResponseBody)
}

// requestHash is the idempotency fingerprint: method + path + canonical query +
// Content-Type + body. The query is canonicalized through url.Values.Encode(),
// whose key order is stable, so reordering parameters does not change the
// fingerprint. The caller dimension lives in the scope rather than the hash, and
// the org dimension is already part of the path.
func requestHash(r *http.Request, body []byte) string {
	h := sha256.New()
	for _, part := range []string{r.Method, r.URL.Path, r.URL.Query().Encode(), r.Header.Get("Content-Type")} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// captureWriter tees the response into a buffer for storage; past the size cap
// it stops buffering and sets overflow.
type captureWriter struct {
	http.ResponseWriter
	buf      *bytes.Buffer
	code     int
	overflow bool
}

// Unwrap lets http.ResponseController reach the underlying writer through this
// wrapper (same contract as statusWriter).
func (w *captureWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *captureWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *captureWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	if !w.overflow {
		if w.buf.Len()+len(b) > maxCapturedBody {
			w.overflow = true
			w.buf.Reset()
		} else {
			w.buf.Write(b)
		}
	}
	return w.ResponseWriter.Write(b)
}

func (w *captureWriter) status() int {
	if w.code == 0 {
		return http.StatusOK
	}
	return w.code
}
