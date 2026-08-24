package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	communitybootstrap "github.com/fairlb/fairlb/internal/community/bootstrap"
)

// send issues a request the way a browser would, including the same-host Origin
// that CSRFGuardSameHost requires on writes.
func send(t *testing.T, r http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = "gw.test"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://gw.test")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func meta(t *testing.T, r http.Handler) (state string, requiresToken bool) {
	t.Helper()
	w := send(t, r, http.MethodGet, "/api/staff/v1/meta", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /meta answered %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		SetupState         string `json:"setup_state"`
		SetupRequiresToken bool   `json:"setup_requires_token"`
		Version            string `json:"version"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.SetupState, body.SetupRequiresToken
}

// The whole point of the wizard: a fresh instance can be taken over by whoever
// reaches it first, and that person ends up signed in — no second command, no
// password they have not typed.
func TestSetupCreatesTheFirstAdministratorAndSignsThemIn(t *testing.T) {
	r, _ := newCERouter(t)

	if state, _ := meta(t, r); state != "available" {
		t.Fatalf("a fresh instance reports setup_state=%q, want available", state)
	}

	w := send(t, r, http.MethodPost, "/api/staff/v1/setup",
		`{"email":"owner@example.com","password":"a-long-enough-password"}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("setup answered %d: %s", w.Code, w.Body.String())
	}

	// Signed in means a session cookie came back and it actually works.
	var session *http.Cookie
	for _, c := range w.Result().Cookies() {
		if strings.Contains(c.Name, "flb_staff") {
			session = c
		}
	}
	if session == nil {
		t.Fatal("setup returned no session cookie; the operator would land on a sign-in form")
	}
	req := httptest.NewRequest(http.MethodGet, "/api/staff/v1/auth/me", nil)
	req.Host = "gw.test"
	req.AddCookie(session)
	whoami := httptest.NewRecorder()
	r.ServeHTTP(whoami, req)
	if whoami.Code != http.StatusOK {
		t.Fatalf("the cookie from setup does not authenticate: %d %s", whoami.Code, whoami.Body.String())
	}

	// And the wizard closes behind them.
	if state, _ := meta(t, r); state != "complete" {
		t.Fatalf("after setup, setup_state=%q, want complete", state)
	}
}

// Answering 204 to the second caller would tell them they own an instance that
// belongs to whoever got there first.
func TestSetupIsRefusedOnceTheInstanceHasAnAdministrator(t *testing.T) {
	r, pool := newCERouter(t)
	if err := communitybootstrap.CreateAdmin(context.Background(), pool,
		"first@example.com", "a-long-enough-password", ""); err != nil {
		t.Fatal(err)
	}
	if state, _ := meta(t, r); state != "complete" {
		t.Fatalf("setup_state=%q with an administrator present, want complete", state)
	}
	w := send(t, r, http.MethodPost, "/api/staff/v1/setup",
		`{"email":"second@example.com","password":"a-long-enough-password"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("a second setup answered %d, want 409: %s", w.Code, w.Body.String())
	}
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM staff_users`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("%d administrators exist after a refused setup, want 1", n)
	}
}

// Both endpoints have to work before anyone can be signed in, which is exactly
// why they are worth a test: an allowlist that misses them leaves an instance
// nobody can ever take ownership of.
func TestSetupAndMetaAreReachableAnonymously(t *testing.T) {
	r, _ := newCERouter(t)
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/staff/v1/meta", ""},
		{http.MethodPost, "/api/staff/v1/setup", `{"email":"a@example.com","password":"a-long-enough-password"}`},
	} {
		w := send(t, r, tc.method, tc.path, tc.body)
		if w.Code == http.StatusUnauthorized {
			t.Errorf("%s %s answered 401 anonymously", tc.method, tc.path)
		}
	}
}

// A password this account can be brute-forced on is the whole security of the
// instance: there is no second factor and no other way in.
func TestSetupRejectsAShortPassword(t *testing.T) {
	r, pool := newCERouter(t)
	w := send(t, r, http.MethodPost, "/api/staff/v1/setup",
		`{"email":"owner@example.com","password":"short"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("a 5-character password answered %d, want 400: %s", w.Code, w.Body.String())
	}
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM staff_users`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d accounts were created despite the rejection", n)
	}
}

func TestSetupTokenGatesTheWizardWhenConfigured(t *testing.T) {
	r, pool := newCERouterWithSetupToken(t, "s3cret-setup-token")

	state, requiresToken := meta(t, r)
	if state != "available" {
		t.Fatalf("setup_state=%q, want available", state)
	}
	// The form cannot render a field it does not know about.
	if !requiresToken {
		t.Error("setup_requires_token is false while a token is configured")
	}

	for name, body := range map[string]string{
		"no token":    `{"email":"a@example.com","password":"a-long-enough-password"}`,
		"wrong token": `{"email":"a@example.com","password":"a-long-enough-password","token":"nope"}`,
	} {
		w := send(t, r, http.MethodPost, "/api/staff/v1/setup", body)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s answered %d, want 403: %s", name, w.Code, w.Body.String())
		}
	}
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM staff_users`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d accounts were created by rejected attempts", n)
	}

	// The right token still gets through — otherwise this test would pass for
	// an endpoint that refuses everyone.
	w := send(t, r, http.MethodPost, "/api/staff/v1/setup",
		`{"email":"a@example.com","password":"a-long-enough-password","token":"s3cret-setup-token"}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("the correct token answered %d, want 204: %s", w.Code, w.Body.String())
	}
}

// With no token configured the wizard is open, which is the default and the
// reason the token exists as an option rather than a requirement.
func TestSetupIsOpenWhenNoTokenIsConfigured(t *testing.T) {
	r, _ := newCERouter(t)
	if _, requiresToken := meta(t, r); requiresToken {
		t.Error("setup_requires_token is true with no token configured")
	}
	w := send(t, r, http.MethodPost, "/api/staff/v1/setup",
		`{"email":"a@example.com","password":"a-long-enough-password"}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("setup answered %d without a token, want 204: %s", w.Code, w.Body.String())
	}
}
