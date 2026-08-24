package httpx_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/foundation/testutil/testpg"
)

func TestIdempotencyReplay(t *testing.T) {
	pool := testpg.Start(t)

	var executions atomic.Int32
	h := httpx.RequestID(httpx.AuthDomain(httpx.AnonymousAuthenticator{Scope: "console"})(
		httpx.Idempotency(pool)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			executions.Add(1)
			w.Header().Set("Location", "/things/42")
			w.Header().Set("Set-Cookie", "session=secret; HttpOnly")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"42"}`))
		}))))

	post := func(key, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/things", strings.NewReader(body))
		if key != "" {
			req.Header.Set(httpx.HeaderIdempotencyKey, key)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// First attempt executes.
	rec := post("key-1", `{"n":1}`)
	if rec.Code != 201 || executions.Load() != 1 {
		t.Fatalf("the first attempt should execute: code=%d exec=%d", rec.Code, executions.Load())
	}

	// Same key, same body: replayed, not executed again.
	rec = post("key-1", `{"n":1}`)
	if rec.Code != 201 || executions.Load() != 1 {
		t.Fatalf("a repeat should replay without executing: code=%d exec=%d", rec.Code, executions.Load())
	}
	if rec.Header().Get("Idempotency-Replayed") != "true" || rec.Header().Get("Location") != "/things/42" {
		t.Errorf("the replay should restore the response headers: %v", rec.Header())
	}
	if rec.Header().Get("Set-Cookie") != "" {
		t.Errorf("Set-Cookie must not be replayed; it would resurrect a session credential: %v", rec.Header())
	}
	if rec.Body.String() != `{"id":"42"}` {
		t.Errorf("the replayed body is wrong: %s", rec.Body.String())
	}

	// Same key, different body: 422.
	rec = post("key-1", `{"n":2}`)
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), errcode.CommonIdempotencyMismatch) {
		t.Errorf("the same key with a different body should be 422: %d %s", rec.Code, rec.Body.String())
	}
	if executions.Load() != 1 {
		t.Errorf("a different body must not execute: %d", executions.Load())
	}

	// A POST without a key is not covered by idempotency at all.
	rec = post("", `{"n":3}`)
	if rec.Code != 201 || executions.Load() != 2 {
		t.Errorf("without a key every request should execute: code=%d exec=%d", rec.Code, executions.Load())
	}
}

// A keyed request over 1 MiB must be rejected with 413 and must never reach the
// handler with a truncated body.
func TestIdempotencyOversizedBodyRejected(t *testing.T) {
	pool := testpg.Start(t)

	var executions atomic.Int32
	h := httpx.RequestID(httpx.AuthDomain(httpx.AnonymousAuthenticator{Scope: "console"})(
		httpx.Idempotency(pool)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			executions.Add(1)
			w.WriteHeader(http.StatusCreated)
		}))))

	big := strings.Repeat("x", 1<<20+1)
	req := httptest.NewRequest("POST", "/things", strings.NewReader(big))
	req.Header.Set(httpx.HeaderIdempotencyKey, "big")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 413 || !strings.Contains(rec.Body.String(), errcode.CommonPayloadTooLarge) {
		t.Fatalf("over the limit should be a 413 problem: %d %s", rec.Code, rec.Body.String())
	}
	if executions.Load() != 0 {
		t.Fatalf("an oversized request must not reach the handler: %d", executions.Load())
	}

	// A normal-sized request still works, and the rejected one left no claim behind.
	req = httptest.NewRequest("POST", "/things", strings.NewReader(`{}`))
	req.Header.Set(httpx.HeaderIdempotencyKey, "big")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 201 || executions.Load() != 1 {
		t.Fatalf("a following normal request should execute: %d exec=%d", rec.Code, executions.Load())
	}
}

// Exceeding the plane-level BodyLimit surfaces as a 413 when the idempotency
// middleware reads the body.
func TestBodyLimitMapsTo413(t *testing.T) {
	pool := testpg.Start(t)

	var executions atomic.Int32
	h := httpx.RequestID(httpx.BodyLimit(16)(httpx.AuthDomain(httpx.AnonymousAuthenticator{Scope: "console"})(
		httpx.Idempotency(pool)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			executions.Add(1)
			w.WriteHeader(http.StatusCreated)
		})))))

	req := httptest.NewRequest("POST", "/things", strings.NewReader(strings.Repeat("x", 64)))
	req.Header.Set(httpx.HeaderIdempotencyKey, "limited")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 413 || !strings.Contains(rec.Body.String(), errcode.CommonPayloadTooLarge) {
		t.Fatalf("exceeding BodyLimit should be a 413 problem: %d %s", rec.Code, rec.Body.String())
	}
	if executions.Load() != 0 {
		t.Fatalf("an oversized request must not reach the handler: %d", executions.Load())
	}
}

// After a first attempt dies (in_flight goes stale), a retry with the same
// fingerprint takes over and executes, instead of getting 409 until the TTL.
func TestIdempotencyStaleInFlightTakeover(t *testing.T) {
	ctx := context.Background()
	pool := testpg.Start(t)

	var executions atomic.Int32
	h := httpx.RequestID(httpx.AuthDomain(httpx.AnonymousAuthenticator{Scope: "console"})(
		httpx.Idempotency(pool)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			executions.Add(1)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))))

	// Simulate the leftovers of a crash: let a hung first attempt write its own
	// in_flight row, then move that row's updated_at into the past.
	req := httptest.NewRequest("POST", "/things", strings.NewReader(`{}`))
	req.Header.Set(httpx.HeaderIdempotencyKey, "crashed")
	started := make(chan struct{})
	release := make(chan struct{})
	slow := httpx.RequestID(httpx.AuthDomain(httpx.AnonymousAuthenticator{Scope: "console"})(
		httpx.Idempotency(pool)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			close(started)
			<-release // a first attempt that never finishes
		}))))
	go slow.ServeHTTP(httptest.NewRecorder(), req)
	<-started

	// Move the in_flight row's updated_at back. The set_updated_at trigger would
	// overwrite a plain UPDATE, so the trigger is disabled for this session with
	// session_replication_role=replica (the test database user is a superuser).
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		"SET session_replication_role = replica",
		"UPDATE idempotency_keys SET updated_at = now() - interval '2 minutes' WHERE idempotency_key = 'crashed'",
		"SET session_replication_role = DEFAULT",
	} {
		if _, err := conn.Exec(ctx, sql); err != nil {
			conn.Release()
			t.Fatalf("move updated_at back (%s): %v", sql, err)
		}
	}
	conn.Release()

	retry := httptest.NewRequest("POST", "/things", strings.NewReader(`{}`))
	retry.Header.Set(httpx.HeaderIdempotencyKey, "crashed")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, retry)
	close(release)

	if rec.Code != 201 || executions.Load() != 1 {
		t.Fatalf("a stale first attempt should be taken over and executed: code=%d exec=%d", rec.Code, executions.Load())
	}

	// The taker-over has finished, so a further retry replays.
	retry2 := httptest.NewRequest("POST", "/things", strings.NewReader(`{}`))
	retry2.Header.Set(httpx.HeaderIdempotencyKey, "crashed")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, retry2)
	if rec.Code != 201 || rec.Header().Get("Idempotency-Replayed") != "true" || executions.Load() != 1 {
		t.Fatalf("once the takeover has finished it should replay: code=%d exec=%d", rec.Code, executions.Load())
	}
}

// Fingerprint dimensions: the canonicalized query and the Content-Type are part
// of the fingerprint. Key order does not matter; a changed value or type is 422.
func TestIdempotencyFingerprintDimensions(t *testing.T) {
	pool := testpg.Start(t)

	var executions atomic.Int32
	h := httpx.RequestID(httpx.AuthDomain(httpx.AnonymousAuthenticator{Scope: "console"})(
		httpx.Idempotency(pool)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			executions.Add(1)
			w.WriteHeader(http.StatusCreated)
		}))))

	post := func(target, contentType string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", target, strings.NewReader(`{}`))
		req.Header.Set(httpx.HeaderIdempotencyKey, "fp")
		req.Header.Set("Content-Type", contentType)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	if rec := post("/things?a=1&b=2", "application/json"); rec.Code != 201 || executions.Load() != 1 {
		t.Fatalf("the first attempt should execute: %d exec=%d", rec.Code, executions.Load())
	}
	// Different query key order canonicalizes to the same fingerprint: replay.
	rec := post("/things?b=2&a=1", "application/json")
	if rec.Code != 201 || rec.Header().Get("Idempotency-Replayed") != "true" || executions.Load() != 1 {
		t.Fatalf("a different key order should replay: %d exec=%d", rec.Code, executions.Load())
	}
	// Different query value: 422.
	if rec := post("/things?a=9&b=2", "application/json"); rec.Code != 422 || !strings.Contains(rec.Body.String(), errcode.CommonIdempotencyMismatch) {
		t.Fatalf("a changed query should be 422: %d %s", rec.Code, rec.Body.String())
	}
	// Different Content-Type: 422.
	if rec := post("/things?a=1&b=2", "text/plain"); rec.Code != 422 {
		t.Fatalf("a changed Content-Type should be 422: %d", rec.Code)
	}
	if executions.Load() != 1 {
		t.Fatalf("a variant must not execute: %d", executions.Load())
	}
}

func TestIdempotencyInFlight(t *testing.T) {
	pool := testpg.Start(t)

	release := make(chan struct{})
	started := make(chan struct{})
	h := httpx.RequestID(httpx.AuthDomain(httpx.AnonymousAuthenticator{Scope: "console"})(
		httpx.Idempotency(pool)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			close(started)
			<-release
			w.WriteHeader(http.StatusCreated)
		}))))

	go func() {
		req := httptest.NewRequest("POST", "/things", strings.NewReader(`{}`))
		req.Header.Set(httpx.HeaderIdempotencyKey, "slow")
		h.ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-started

	req := httptest.NewRequest("POST", "/things", strings.NewReader(`{}`))
	req.Header.Set(httpx.HeaderIdempotencyKey, "slow")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	close(release)

	if rec.Code != 409 || !strings.Contains(rec.Body.String(), errcode.CommonIdempotencyInFlight) {
		t.Errorf("an unfinished first attempt should be 409: %d %s", rec.Code, rec.Body.String())
	}
}

// A failure on our side must not be pinned under the key: the retry the header
// exists for has to execute. A 4xx is the caller's outcome and is replayed.
func TestIdempotencyDoesNotStoreServerFailures(t *testing.T) {
	pool := testpg.Start(t)
	var executions atomic.Int32
	status := atomic.Int32{}
	status.Store(http.StatusInternalServerError)
	h := httpx.RequestID(httpx.AuthDomain(httpx.AnonymousAuthenticator{Scope: "console"})(
		httpx.Idempotency(pool)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			executions.Add(1)
			w.WriteHeader(int(status.Load()))
			_, _ = w.Write([]byte(`{"n":1}`))
		}))))
	post := func(key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/things", strings.NewReader(`{}`))
		req.Header.Set(httpx.HeaderIdempotencyKey, key)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := post("k"); rec.Code != 500 || executions.Load() != 1 {
		t.Fatalf("first attempt: %d exec=%d", rec.Code, executions.Load())
	}
	status.Store(http.StatusTooManyRequests)
	if rec := post("k"); rec.Code != 429 || executions.Load() != 2 {
		t.Fatalf("a 500 must not be replayed: %d exec=%d", rec.Code, executions.Load())
	}
	status.Store(http.StatusCreated)
	if rec := post("k"); rec.Code != 201 || executions.Load() != 3 {
		t.Fatalf("a 429 must not be replayed: %d exec=%d", rec.Code, executions.Load())
	}
	if rec := post("k"); rec.Code != 201 || executions.Load() != 3 || rec.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("the 201 is the outcome and is replayed: %d exec=%d", rec.Code, executions.Load())
	}
	status.Store(http.StatusUnprocessableEntity)
	if rec := post("k2"); rec.Code != 422 || executions.Load() != 4 {
		t.Fatalf("4xx first attempt: %d exec=%d", rec.Code, executions.Load())
	}
	if rec := post("k2"); rec.Code != 422 || executions.Load() != 4 {
		t.Fatalf("a 4xx is the caller's outcome and is replayed: %d exec=%d", rec.Code, executions.Load())
	}
}

// Anonymous callers are scoped by client address: a stranger cannot park a key
// that answers 422 (or replays their response) to the next user of the string.
func TestIdempotencyAnonymousScopedByClientAddress(t *testing.T) {
	pool := testpg.Start(t)
	var executions atomic.Int32
	h := httpx.RequestID(httpx.AuthDomain(httpx.AnonymousAuthenticator{Scope: "console"})(
		httpx.Idempotency(pool)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			executions.Add(1)
			w.WriteHeader(http.StatusCreated)
		}))))
	post := func(addr, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/auth/login", strings.NewReader(body))
		req.RemoteAddr = addr
		req.Header.Set(httpx.HeaderIdempotencyKey, "retry-1")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := post("192.0.2.10:1", `{"who":"attacker"}`); rec.Code != 201 || executions.Load() != 1 {
		t.Fatalf("attacker: %d exec=%d", rec.Code, executions.Load())
	}
	// A different address with the same key and a different body executes
	// instead of colliding with the parked key.
	if rec := post("192.0.2.20:1", `{"who":"victim"}`); rec.Code != 201 || executions.Load() != 2 {
		t.Fatalf("another address must not collide: %d exec=%d", rec.Code, executions.Load())
	}
	// The same address still gets the idempotency contract.
	if rec := post("192.0.2.20:1", `{"who":"victim"}`); rec.Code != 201 || executions.Load() != 2 {
		t.Fatalf("same address replays: %d exec=%d", rec.Code, executions.Load())
	}
	if rec := post("192.0.2.20:1", `{"who":"other"}`); rec.Code != 422 {
		t.Fatalf("same address, different body: %d", rec.Code)
	}
}
