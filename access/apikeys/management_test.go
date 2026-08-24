package apikeys_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/access/apikeys"
	"github.com/fairlb/fairlb/foundation/crypto"
	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/foundation/publicid"
)

// Authentication of management keys. This is an externally reachable
// authentication surface, so there are more negative cases than positive ones:
// every "this must not be accepted as a management key" needs its own case, or
// it is a silent path to more permission than intended.

// anonSession stands in for an underlying authenticator with no session.
type anonSession struct{}

func (anonSession) Authenticate(*http.Request) (httpx.Principal, error) {
	return httpx.Principal{Scope: "console"}, nil
}

// userSession stands in for an underlying authenticator with a session.
type userSession struct{ subject string }

func (u userSession) Authenticate(*http.Request) (httpx.Principal, error) {
	return httpx.Principal{Scope: "console", Subject: u.subject}, nil
}

func TestManagementKeyAuthenticates(t *testing.T) {
	f, org, actor := newUpdateFixture(t)
	plaintext := f.mkMgmtKey(t, org, actor, apikeys.ScopeKeysRW)

	a := apikeys.NewManagementAuthenticator(anonSession{}, f.pool)
	p, err := a.Authenticate(bearerReq(plaintext))
	if err != nil {
		t.Fatal(err)
	}
	if p.Subject == "" {
		t.Fatal("a key carrying a management scope should authenticate")
	}
	if p.OrgID != publicid.UUIDString(org) {
		t.Errorf("the subject should be pinned to the org that issued it: want %s, got %s", publicid.UUIDString(org), p.OrgID)
	}
	if !p.HasScope(apikeys.ScopeKeysRW) {
		t.Errorf("the scopes should be carried through, got %v", p.Scopes)
	}
	if !p.IsCredential() {
		t.Error("a management key should be recognized as a credential subject")
	}
}

// Each of these must fall back to anonymous. They are the entrances to a
// privilege path, so they are pinned one by one.
func TestManagementKeyRejects(t *testing.T) {
	f, org, actor := newUpdateFixture(t)
	ctx := context.Background()

	inferenceOnly := f.mkMgmtKey(t, org, actor, apikeys.ScopeInference)
	revoked := f.mkMgmtKey(t, org, actor, apikeys.ScopeKeysRW)
	revokedRow := f.keyByPlaintext(t, revoked)
	if err := f.svc.Revoke(ctx, org, actor, revokedRow.ID); err != nil {
		t.Fatal(err)
	}
	expired := f.mkMgmtKey(t, org, actor, apikeys.ScopeKeysRW)
	expiredRow := f.keyByPlaintext(t, expired)
	past := time.Now().Add(-time.Hour)
	if _, err := f.svc.Update(ctx, apikeys.UpdateInput{
		OrgID: org, ActorID: actor, KeyID: expiredRow.ID, ExpiresAt: &past,
	}); err != nil {
		t.Fatal(err)
	}

	cases := map[string]string{
		"inference-only key pointed at the management plane": inferenceOnly,
		"revoked":      revoked,
		"expired":      expired,
		"no such key":  "sk-flb-v1-nosuchkeyatall",
		"wrong prefix": "Bearer-looking-but-not-a-key",
		"empty":        "",
	}
	a := apikeys.NewManagementAuthenticator(anonSession{}, f.pool)
	for name, token := range cases {
		t.Run(name, func(t *testing.T) {
			p, err := a.Authenticate(bearerReq(token))
			if err != nil {
				t.Fatal(err)
			}
			if p.Subject != "" || p.IsCredential() {
				t.Errorf("should fall back to anonymous, got subject=%q org=%q", p.Subject, p.OrgID)
			}
		})
	}
}

// The session wins over the bearer token when both are present: with a person
// driving, permissions should come from the finer-grained membership role rather
// than a key's coarse scopes.
func TestSessionWinsOverBearer(t *testing.T) {
	f, org, actor := newUpdateFixture(t)
	plaintext := f.mkMgmtKey(t, org, actor, apikeys.ScopeKeysRW)

	a := apikeys.NewManagementAuthenticator(userSession{subject: "the-user"}, f.pool)
	p, err := a.Authenticate(bearerReq(plaintext))
	if err != nil {
		t.Fatal(err)
	}
	if p.Subject != "the-user" || p.IsCredential() {
		t.Errorf("the session should win, got subject=%q org=%q", p.Subject, p.OrgID)
	}
}

// A management key may only mint inference keys, never another management key:
// that would be privilege escalation.
func TestManagementKeyCannotMintManagementKey(t *testing.T) {
	f, org, actor := newUpdateFixture(t)
	ctx := withCredential(context.Background(), org)

	_, row, err := f.svc.Create(ctx, apikeys.CreateInput{
		OrgID: org, ActorID: actor, Name: "minted by a management key",
		Scopes: []string{apikeys.ScopeKeysRW}, // asking for escalated scopes
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range row.Scopes {
		if s != apikeys.ScopeInference {
			t.Errorf("a key minted by a management key must only have the inference scope, got %v — that is escalation", row.Scopes)
		}
	}
}

// A credential subject can only touch its own org — defense in depth, which
// holds even if the gate in front of it were bypassed.
func TestCredentialCannotTouchOtherOrg(t *testing.T) {
	f, org, actor := newUpdateFixture(t)
	other := f.org(t, "cred-other")

	ctx := withCredential(context.Background(), org)
	if _, _, err := f.svc.Create(ctx, apikeys.CreateInput{
		OrgID: other, ActorID: actor, Name: "out of scope",
	}); err == nil {
		t.Error("a credential subject should not be able to create a key in another org")
	}
}

// ===== Fixtures =====

func bearerReq(token string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

// withCredential builds a context carrying a credential subject, as if a
// management key had already passed the gate.
func withCredential(ctx context.Context, org pgtype.UUID) context.Context {
	return httpx.WithPrincipal(ctx, httpx.Principal{
		Scope: "console", Subject: "key-subject",
		Scopes: []string{apikeys.ScopeKeysRW}, OrgID: publicid.UUIDString(org),
	})
}

func (f *fixture) mkMgmtKey(t *testing.T, org, actor pgtype.UUID, scopes ...string) string {
	t.Helper()
	plaintext, _, err := f.svc.Create(context.Background(), apikeys.CreateInput{
		OrgID: org, ActorID: actor, Name: "mgmt-" + scopes[0] + "-" + randSuffix(),
		Scopes: scopes,
	})
	if err != nil {
		t.Fatal(err)
	}
	return plaintext
}

func (f *fixture) keyByPlaintext(t *testing.T, plaintext string) apikeys.Record {
	t.Helper()
	row, err := f.q.RecordByHash(context.Background(), crypto.HashToken(plaintext))
	if err != nil {
		t.Fatal(err)
	}
	return row
}

var randCounter int

func randSuffix() string {
	randCounter++
	return time.Now().Format("150405.000") + "-" + string(rune('a'+randCounter%26))
}

// AuthorizeOrg has four outcomes: a session passes, a mismatched org is 404, a
// missing scope is 403, and everything correct passes. It is the authorization
// gate for management keys — the org binding plus the scope.
func TestAuthorizeOrg(t *testing.T) {
	org := pgtype.UUID{}
	if err := org.Scan("11111111-1111-1111-1111-111111111111"); err != nil {
		t.Fatal(err)
	}
	orgStr := publicid.UUIDString(org)

	// A session subject passes straight through; its authorization comes from
	// the membership role instead.
	sessionCtx := httpx.WithPrincipal(context.Background(), httpx.Principal{Scope: "console", Subject: "user-1"})
	if err := apikeys.AuthorizeOrg(sessionCtx, org, apikeys.ScopeKeysRW); err != nil {
		t.Fatalf("a session subject should be allowed: %v", err)
	}

	// Mismatched org: 404, the same as "no such org", so an unauthorized key is
	// never told whether it exists.
	otherCtx := httpx.WithPrincipal(context.Background(), httpx.Principal{
		Scope: "console", Subject: "key-1", OrgID: "22222222-2222-2222-2222-222222222222",
		Scopes: []string{apikeys.ScopeKeysRW},
	})
	wantCode(t, apikeys.AuthorizeOrg(otherCtx, org, apikeys.ScopeKeysRW), errcode.CommonNotFound)

	// Right org, missing scope: 403.
	noScopeCtx := httpx.WithPrincipal(context.Background(), httpx.Principal{
		Scope: "console", Subject: "key-1", OrgID: orgStr,
		Scopes: []string{apikeys.ScopeUsageRead},
	})
	wantCode(t, apikeys.AuthorizeOrg(noScopeCtx, org, apikeys.ScopeKeysRW), errcode.CommonForbidden)

	// Right org and scope: allowed.
	okCtx := httpx.WithPrincipal(context.Background(), httpx.Principal{
		Scope: "console", Subject: "key-1", OrgID: orgStr,
		Scopes: []string{apikeys.ScopeKeysRW},
	})
	if err := apikeys.AuthorizeOrg(okCtx, org, apikeys.ScopeKeysRW); err != nil {
		t.Fatalf("everything correct should be allowed: %v", err)
	}
}
