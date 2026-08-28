// Behaviour tests for the self-hosted build. They run against the schema this
// build actually ships (testpg.Start migrates core + product and nothing else),
// which is the point: on the hosted schema staff_users.totp_secret_enc is NOT
// NULL, a column this build never creates, and creating an account would fail
// on it. Code has to be tested against the world it will meet.
package community_test

import (
	"context"
	"errors"
	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/gateway"
	"github.com/fairlb/fairlb/settings"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/access/apikeys"
	"github.com/fairlb/fairlb/access/keyfmt"
	"github.com/fairlb/fairlb/access/organizations"
	"github.com/fairlb/fairlb/foundation/db"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/foundation/publicid"
	"github.com/fairlb/fairlb/foundation/testutil/testpg"
	communitybootstrap "github.com/fairlb/fairlb/internal/community/bootstrap"
	communitysettle "github.com/fairlb/fairlb/internal/community/settle"
	communitystaffapi "github.com/fairlb/fairlb/internal/community/staffapi"
	communitystaffauth "github.com/fairlb/fairlb/internal/community/staffauth"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
	"github.com/fairlb/fairlb/internal/gateway/proxy"
	gwsettle "github.com/fairlb/fairlb/internal/gateway/settle"
)

// The default organisation is idempotent: a restart must not change its id, or
// historical usage and every key become orphans.
func TestEnsureDefaultOrgIsIdempotent(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()

	first, err := communitybootstrap.EnsureDefaultOrg(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	second, err := communitybootstrap.EnsureDefaultOrg(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("the default organisation id must be stable: %v then %v", first, second)
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM orgs WHERE slug = $1`, communitybootstrap.DefaultOrgSlug).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("there should be exactly one default organisation row, got %d", n)
	}
}

// A newly created account is a superadmin, and that is stored in the row: the
// gateway's superadmin check reads the role from the row, and hard-coding it in
// the code path would break the property that one copy of the decision logic
// serves every deployment shape.
func TestCreateAdminIsSuperadminInDB(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()

	if exists, err := communitybootstrap.AdminExists(ctx, pool); err != nil || exists {
		t.Fatalf("an empty database should have no administrator: exists=%v err=%v", exists, err)
	}
	if err := communitybootstrap.CreateAdmin(ctx, pool, "admin@example.com", "correct-horse", "Admin"); err != nil {
		t.Fatal(err)
	}
	var role, status string
	if err := pool.QueryRow(ctx,
		`SELECT role, status FROM staff_users WHERE email = 'admin@example.com'`).Scan(&role, &status); err != nil {
		t.Fatal(err)
	}
	if role != "superadmin" || status != "active" {
		t.Fatalf("the administrator should be an active superadmin, got %s/%s", role, status)
	}
	if exists, err := communitybootstrap.AdminExists(ctx, pool); err != nil || !exists {
		t.Fatalf("after creation it should report existence: exists=%v err=%v", exists, err)
	}
}

// Log in, resolve the session, get a superadmin principal. A wrong password and
// an unknown account produce the same error.
func TestLoginAndAuthenticate(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()
	svc := communitystaffauth.New(pool)

	if err := communitybootstrap.CreateAdmin(ctx, pool, "ops@example.com", "correct-horse", "Ops"); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Login(ctx, "ops@example.com", "wrong"); err == nil {
		t.Fatal("a wrong password must fail")
	}
	if _, err := svc.Login(ctx, "nobody@example.com", "correct-horse"); err == nil {
		t.Fatal("an unknown account must fail")
	}

	token, err := svc.Login(ctx, "ops@example.com", "correct-horse")
	if err != nil {
		t.Fatal(err)
	}

	auth := communitystaffauth.NewAuthenticator(svc, true)
	r := httptest.NewRequest(http.MethodGet, "/api/staff/v1/anything", nil)
	r.AddCookie(&http.Cookie{Name: communitystaffauth.SessionCookie, Value: token})
	p, err := auth.Authenticate(r)
	if err != nil {
		t.Fatal(err)
	}
	// The role has to be superadmin: it is what the gateway's pricing write
	// gate reads.
	if p.Scope != "admin" || p.Role != "superadmin" || p.Subject == "" {
		t.Fatalf("wrong principal shape: %+v", p)
	}

	// After logout the same token resolves to an anonymous principal with an
	// empty subject rather than an error, because the public login endpoint
	// has to stay reachable.
	if err := svc.Logout(ctx, token); err != nil {
		t.Fatal(err)
	}
	if p2, err := auth.Authenticate(r); err != nil || p2.Subject != "" {
		t.Fatalf("after logout the principal should be anonymous: %+v err=%v", p2, err)
	}
}

// No cookie and a garbage cookie both resolve to an anonymous principal rather
// than an error.
func TestAuthenticateAnonymousPaths(t *testing.T) {
	pool := testpg.Start(t)
	auth := communitystaffauth.NewAuthenticator(communitystaffauth.New(pool), true)

	for _, tc := range []struct{ name, cookie string }{
		{"no cookie", ""},
		{"garbage token", "not-a-real-token"},
		{"wrong cookie name", "wrong-cookie-name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/x", nil)
			if tc.cookie != "" {
				name := communitystaffauth.SessionCookie
				if tc.name == "wrong cookie name" {
					name = communitystaffauth.SessionCookieInsecure
				}
				r.AddCookie(&http.Cookie{Name: name, Value: tc.cookie})
			}
			p, err := auth.Authenticate(r)
			if err != nil {
				t.Fatalf("the anonymous path must not error: %v", err)
			}
			if p.Subject != "" || p.Scope != "admin" {
				t.Fatalf("expected an anonymous admin principal: %+v", p)
			}
		})
	}
}

// There is no balance here, but per-key spend is still accumulated -- the
// spend-limit enforcement on the request path depends on it.
func TestSettlerAccumulatesKeySpend(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()

	org, err := communitybootstrap.EnsureDefaultOrg(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	var keyID pgtype.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO api_keys (org_id, name, prefix, key_hash)
		VALUES ($1, 'k', 'sk-flb-1', 'hash-w9') RETURNING id`, org).Scan(&keyID); err != nil {
		t.Fatal(err)
	}

	s := communitysettle.New(pool)
	// Hold and Void are no-ops but must not error: the data plane calls them
	// on every request.
	holdID, err := s.Hold(ctx, gwsettle.HoldInput{OrgID: org, RequestID: "r1", AmountNano: 1})
	if err != nil {
		t.Fatalf("Hold should be a no-op: %v", err)
	}
	// The zero id is the honest answer, not a placeholder: there is no
	// reservation here, so the usage row's hold column stays NULL rather than
	// naming something that does not exist.
	if holdID.Valid {
		t.Errorf("a deployment without reservations should return no id: %x", holdID.Bytes)
	}
	if err := s.Void(ctx, org, "r1"); err != nil {
		t.Fatalf("Void should be a no-op: %v", err)
	}
	// ProtectHold always reports the reservation as gone, which is how the
	// caller knows to skip the follow-up queue.
	if ok, err := s.ProtectHold(ctx, org, "r1"); err != nil || ok {
		t.Fatalf("ProtectHold should return (false, nil), got (%v, %v)", ok, err)
	}

	// Two settlements accumulate into the same day.
	for _, amount := range []int64{1_000_000, 2_500_000} {
		if err := db.WithSystemTx(ctx, pool, func(tx pgx.Tx) error {
			return s.SettleTx(ctx, tx, gwsettle.SettleInput{
				OrgID: org, RequestID: "r1", ActualNano: amount, APIKeyID: keyID,
			})
		}); err != nil {
			t.Fatal(err)
		}
	}

	var daily, total int64
	if err := pool.QueryRow(ctx,
		`SELECT spent_nano FROM api_key_daily_spend WHERE api_key_id = $1`, keyID).Scan(&daily); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT total_spent_nano FROM api_keys WHERE id = $1`, keyID).Scan(&total); err != nil {
		t.Fatal(err)
	}
	const want = 3_500_000
	if daily != want || total != want {
		t.Fatalf("the accumulated spend should be %d: daily=%d total=%d", want, daily, total)
	}
}

// End to end across the three endpoints: login issues a fully attributed
// cookie, the identity endpoint recognises it, and logout revokes it.
//
// The cookie attributes are asserted separately: HttpOnly, SameSite and the
// __Host- prefix contract are a security boundary guaranteed by the custom
// response type -- and "forgot to set them" is functionally identical to "set
// them" (you still log in successfully), so only an assertion can catch it.
func TestCommunityStaffAPIEndToEnd(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()
	svc := communitystaffauth.New(pool)
	if err := communitybootstrap.CreateAdmin(ctx, pool, "api@example.com", "correct-horse", "API"); err != nil {
		t.Fatal(err)
	}

	org, err := communitybootstrap.EnsureDefaultOrg(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	keys := apikeys.NewService(apikeys.ServiceConfig{Database: pool, Admin: communitystaffapi.AllowKeyAdmin})
	srv := communitystaffapi.NewServer(pool, svc, keys, org, nil /* no cache */, true, /* secure */
		communitystaffapi.SetupConfig{Version: "test"}, nil)

	// 1. Log in.
	body := communitystaffapi.CommunityLoginJSONRequestBody{Email: "api@example.com", Password: "correct-horse"}
	resp, err := srv.CommunityLogin(ctx, communitystaffapi.CommunityLoginRequestObject{Body: &body})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	rec := httptest.NewRecorder()
	if err := resp.VisitCommunityLoginResponse(rec); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("login should be 204, got %d", rec.Code)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("exactly one cookie should be issued, got %d", len(cookies))
	}
	c := cookies[0]
	if c.Name != communitystaffauth.SessionCookie {
		t.Errorf("the cookie should be named %s, got %s", communitystaffauth.SessionCookie, c.Name)
	}
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" {
		t.Errorf("cookie attributes are incomplete: HttpOnly=%v Secure=%v SameSite=%v Path=%q",
			c.HttpOnly, c.Secure, c.SameSite, c.Path)
	}
	if c.Value == "" {
		t.Fatal("the cookie value is empty")
	}

	// 2. Identity: resolve the session into a principal, then ask who it is.
	auth := communitystaffauth.NewAuthenticator(svc, true)
	r := httptest.NewRequest(http.MethodGet, "/api/staff/v1/auth/me", nil)
	r.AddCookie(c)
	p, err := auth.Authenticate(r)
	if err != nil {
		t.Fatal(err)
	}
	meResp, err := srv.CommunityMe(httpx.WithPrincipal(ctx, p), communitystaffapi.CommunityMeRequestObject{})
	if err != nil {
		t.Fatalf("me: %v", err)
	}
	me, ok := meResp.(communitystaffapi.CommunityMe200JSONResponse)
	if !ok {
		t.Fatalf("the identity endpoint should be 200, got %T", meResp)
	}
	if string(me.Email) != "api@example.com" || me.Role != communitystaffapi.Superadmin {
		t.Errorf("wrong identity: %+v", me)
	}

	// 3. Log out: the session dies and the same cookie resolves to anonymous.
	outResp, err := srv.CommunityLogout(httpx.WithPrincipal(ctx, p), communitystaffapi.CommunityLogoutRequestObject{})
	if err != nil {
		t.Fatalf("logout: %v", err)
	}
	outRec := httptest.NewRecorder()
	if err := outResp.VisitCommunityLogoutResponse(outRec); err != nil {
		t.Fatal(err)
	}
	if got := outRec.Result().Cookies(); len(got) != 1 || got[0].MaxAge != -1 {
		t.Errorf("logout should clear the cookie with MaxAge=-1, got %+v", got)
	}
	if p2, err := auth.Authenticate(r); err != nil || p2.Subject != "" {
		t.Fatalf("after logout it should be anonymous: %+v err=%v", p2, err)
	}

	// 4. With no identity, the identity endpoint returns 401, not an empty
	//    object.
	if _, err := srv.CommunityMe(ctx, communitystaffapi.CommunityMeRequestObject{}); err == nil {
		t.Error("with no principal the identity endpoint must error")
	}
}

// Whether a usable API key can actually be issued -- the minimum condition for
// this being a product at all.
//
// A multi-tenant deployment manages keys from a per-organisation self-service
// surface gated on membership roles; there is no organization layer here, so it goes
// through the administrator endpoints instead. The key material logic in
// between is shared, and what this case really checks is that the whole of it
// still works on this schema once "who may manage" becomes always-permit.
func TestCommunityKeyLifecycle(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()
	org, err := communitybootstrap.EnsureDefaultOrg(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	srv := communitystaffapi.NewServer(pool, communitystaffauth.New(pool),
		apikeys.NewService(apikeys.ServiceConfig{Database: pool, Admin: communitystaffapi.AllowKeyAdmin}), org, nil, true,
		communitystaffapi.SetupConfig{Version: "test"}, nil)

	// 1. Create: the plaintext appears exactly once, and what is stored is a
	//    hash.
	limit := int64(5_000_000_000)
	created, err := srv.CommunityCreateKey(ctx, communitystaffapi.CommunityCreateKeyRequestObject{
		Body: &communitystaffapi.CommunityCreateKeyJSONRequestBody{Name: "prod", SpendLimitNano: &limit},
	})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	out, ok := created.(communitystaffapi.CommunityCreateKey201JSONResponse)
	if !ok {
		t.Fatalf("expected 201, got %T", created)
	}
	if out.Key == "" || !strings.HasPrefix(out.Key, keyfmt.Prefix) {
		t.Fatalf("wrong plaintext shape: %q", out.Key)
	}
	if out.ApiKey.SpendLimitNano == nil || *out.ApiKey.SpendLimitNano != limit {
		t.Errorf("the spend limit was not persisted: %+v", out.ApiKey.SpendLimitNano)
	}

	// 2. The plaintext authenticates against the data plane. Without this step
	//    all we have is a good-looking string.
	auth := proxy.NewAuthenticator(apikeys.NewStore(pool), organizations.New(pool), gwdb.New(pool), nil)
	// authErr is declared separately because Authenticate returns a concrete
	// pointer type. Assigning it into the outer error variable would put a nil
	// pointer inside an interface, and such an interface is not equal to nil --
	// the symptom is authentication succeeding while the test reports failure,
	// printing `<nil>` as the cause.
	id, authErr := auth.Authenticate(ctx, out.Key)
	if authErr != nil {
		t.Fatalf("the newly created key failed to authenticate: %v", authErr)
	}
	if id.OrgID != org {
		t.Errorf("the key should be bound to the default organisation, got %v", id.OrgID)
	}

	// 3. The key shows up in the list.
	listed, err := srv.CommunityListKeys(ctx, communitystaffapi.CommunityListKeysRequestObject{})
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	items := listed.(communitystaffapi.CommunityListKeys200JSONResponse).Items
	if len(items) != 1 || items[0].Name != "prod" {
		t.Fatalf("the list should hold exactly the prod key, got %+v", items)
	}

	// 4. After revocation, authentication fails.
	if _, err := srv.CommunityRevokeKey(ctx, communitystaffapi.CommunityRevokeKeyRequestObject{KeyId: items[0].Id}); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, e := auth.Authenticate(ctx, out.Key); e == nil {
		t.Fatal("a revoked key still authenticated")
	}
}

// Teams: what "these people may use only this model" is said with here.
//
// The chain has to hold end to end -- create a team, issue a key in it, and
// have the data plane resolve that key to that team -- because every
// per-group control the deployment has (the access tier, the rate ceilings) is
// configured per team and reached through that org id. A key that silently
// landed in the first team would be configured by settings nobody applied to
// it.
func TestCommunityTeamsCarryKeys(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()
	first, err := communitybootstrap.EnsureDefaultOrg(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	srv := communitystaffapi.NewServer(pool, communitystaffauth.New(pool),
		apikeys.NewService(apikeys.ServiceConfig{Database: pool, Admin: communitystaffapi.AllowKeyAdmin}), first, nil, true,
		communitystaffapi.SetupConfig{Version: "test"}, nil)

	// The first team is listed, marked as the default, and is the only one.
	listed, err := srv.CommunityListTeams(ctx, communitystaffapi.CommunityListTeamsRequestObject{})
	if err != nil {
		t.Fatalf("list teams: %v", err)
	}
	teams := listed.(communitystaffapi.CommunityListTeams200JSONResponse).Items
	if len(teams) != 1 || !teams[0].IsDefault {
		t.Fatalf("a fresh instance should have exactly one team, marked default: %+v", teams)
	}

	created, err := srv.CommunityCreateTeam(ctx, communitystaffapi.CommunityCreateTeamRequestObject{
		Body: &communitystaffapi.CommunityCreateTeamJSONRequestBody{Name: "Research"},
	})
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	team := communitystaffapi.Team(created.(communitystaffapi.CommunityCreateTeam201JSONResponse))
	if team.IsDefault {
		t.Error("a team created later is not the default one")
	}

	// A key issued into that team belongs to it, both in the response and in
	// what the data plane resolves.
	key, err := srv.CommunityCreateKey(ctx, communitystaffapi.CommunityCreateKeyRequestObject{
		Body: &communitystaffapi.CommunityCreateKeyJSONRequestBody{Name: "research-key", TeamId: &team.Id},
	})
	if err != nil {
		t.Fatalf("create key in a team: %v", err)
	}
	out := key.(communitystaffapi.CommunityCreateKey201JSONResponse)
	if out.ApiKey.TeamId != team.Id {
		t.Errorf("the key should report the team it was issued in, got %q want %q", out.ApiKey.TeamId, team.Id)
	}
	id, authErr := proxy.NewAuthenticator(apikeys.NewStore(pool), organizations.New(pool), gwdb.New(pool), nil).
		Authenticate(ctx, out.Key)
	if authErr != nil {
		t.Fatalf("the key failed to authenticate: %v", authErr)
	}
	if got := publicid.Format(publicid.Org, id.OrgID); got != team.Id {
		t.Errorf("the data plane resolved the key to %s, want the team it was issued in %s", got, team.Id)
	}

	// Listing is per team: the second team's key is not in the first team's
	// list, and omitting the team gives the first one.
	inFirst, err := srv.CommunityListKeys(ctx, communitystaffapi.CommunityListKeysRequestObject{})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(inFirst.(communitystaffapi.CommunityListKeys200JSONResponse).Items); n != 0 {
		t.Errorf("the first team has no keys, got %d", n)
	}
	inTeam, err := srv.CommunityListKeys(ctx, communitystaffapi.CommunityListKeysRequestObject{
		Params: communitystaffapi.CommunityListKeysParams{TeamId: &team.Id},
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(inTeam.(communitystaffapi.CommunityListKeys200JSONResponse).Items); n != 1 {
		t.Errorf("the second team should hold exactly its own key, got %d", n)
	}

	// Editing a key in a second team must work. The shared key service scopes
	// every write by organisation, so a handler that assumed the first team
	// would answer "not found" here -- which reads as "no such key".
	rpm := 60
	if _, err := srv.CommunityUpdateKey(ctx, communitystaffapi.CommunityUpdateKeyRequestObject{
		KeyId: out.ApiKey.Id,
		Body:  &communitystaffapi.CommunityUpdateKeyJSONRequestBody{RateLimitRpm: &rpm},
	}); err != nil {
		t.Fatalf("editing a key in a second team should work: %v", err)
	}
}

// Suspending a team refuses its keys, and the first team cannot be suspended.
//
// The second half is the load-bearing one: the first team is where keys land
// when none is named, so suspending it would take down a deployment that never
// opted into having teams at all.
func TestCommunityTeamSuspension(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()
	first, err := communitybootstrap.EnsureDefaultOrg(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	srv := communitystaffapi.NewServer(pool, communitystaffauth.New(pool),
		apikeys.NewService(apikeys.ServiceConfig{Database: pool, Admin: communitystaffapi.AllowKeyAdmin}), first, nil, true,
		communitystaffapi.SetupConfig{Version: "test"}, nil)

	created, err := srv.CommunityCreateTeam(ctx, communitystaffapi.CommunityCreateTeamRequestObject{
		Body: &communitystaffapi.CommunityCreateTeamJSONRequestBody{Name: "Contractors"},
	})
	if err != nil {
		t.Fatal(err)
	}
	team := communitystaffapi.Team(created.(communitystaffapi.CommunityCreateTeam201JSONResponse))
	key, err := srv.CommunityCreateKey(ctx, communitystaffapi.CommunityCreateKeyRequestObject{
		Body: &communitystaffapi.CommunityCreateKeyJSONRequestBody{Name: "k", TeamId: &team.Id},
	})
	if err != nil {
		t.Fatal(err)
	}
	plaintext := key.(communitystaffapi.CommunityCreateKey201JSONResponse).Key

	auth := proxy.NewAuthenticator(apikeys.NewStore(pool), organizations.New(pool), gwdb.New(pool), nil)
	// Self-check first: the key works while the team is active, so a refusal
	// below is the suspension and not a broken fixture.
	if _, e := auth.Authenticate(ctx, plaintext); e != nil {
		t.Fatalf("the key should work while its team is active: %v", e)
	}

	suspended := communitystaffapi.CommunityUpdateTeamJSONBodyStatusSuspended
	if _, err := srv.CommunityUpdateTeam(ctx, communitystaffapi.CommunityUpdateTeamRequestObject{
		TeamId: team.Id,
		Body:   &communitystaffapi.CommunityUpdateTeamJSONRequestBody{Status: &suspended},
	}); err != nil {
		t.Fatalf("suspend a team: %v", err)
	}
	if _, e := auth.Authenticate(ctx, plaintext); e == nil {
		t.Error("a suspended team's key should be refused")
	}

	// The first team refuses to be suspended, and says why.
	firstID := publicid.Format(publicid.Org, first)
	if _, err := srv.CommunityUpdateTeam(ctx, communitystaffapi.CommunityUpdateTeamRequestObject{
		TeamId: firstID,
		Body:   &communitystaffapi.CommunityUpdateTeamJSONRequestBody{Status: &suspended},
	}); err == nil {
		t.Error("suspending the first team would refuse requests for a deployment that never asked for teams")
	}

	// Reactivating restores the key rather than needing it reissued: that is
	// the whole reason suspension is offered instead of deletion.
	active := communitystaffapi.CommunityUpdateTeamJSONBodyStatusActive
	if _, err := srv.CommunityUpdateTeam(ctx, communitystaffapi.CommunityUpdateTeamRequestObject{
		TeamId: team.Id,
		Body:   &communitystaffapi.CommunityUpdateTeamJSONRequestBody{Status: &active},
	}); err != nil {
		t.Fatal(err)
	}
	if _, e := auth.Authenticate(ctx, plaintext); e != nil {
		t.Errorf("reactivating the team should restore its keys: %v", e)
	}
}

// The settings surface (ADR-0198): this deployment lists exactly the keys it
// assembled, refuses a high-impact write without a reason, and a write with one
// lands and is audited. The server is built the way router.go builds it — with
// the store the entrypoint hands it.
func TestCommunitySettingsSurface(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()
	svc := communitystaffauth.New(pool)
	if err := communitybootstrap.CreateAdmin(ctx, pool, "settings@example.com", "correct-horse", "Settings"); err != nil {
		t.Fatal(err)
	}
	org, err := communitybootstrap.EnsureDefaultOrg(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	keys := apikeys.NewService(apikeys.ServiceConfig{Database: pool, Admin: communitystaffapi.AllowKeyAdmin})
	set := settings.New(pool, nil, settings.NewRegistry(gateway.SettingSpecs()), nil)
	srv := communitystaffapi.NewServer(pool, svc, keys, org, nil, true, communitystaffapi.SetupConfig{Version: "test"}, set)

	var adminID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM staff_users WHERE email = 'settings@example.com'`).Scan(&adminID); err != nil {
		t.Fatal(err)
	}
	actx := httpx.WithPrincipal(ctx, httpx.Principal{Scope: "admin", Subject: adminID, Role: "superadmin"})

	listResp, err := srv.CommunityListSettings(actx, communitystaffapi.CommunityListSettingsRequestObject{})
	if err != nil {
		t.Fatal(err)
	}
	list := listResp.(communitystaffapi.CommunityListSettings200JSONResponse)
	keysSeen := map[string]bool{}
	for _, e := range list.Items {
		keysSeen[e.Key] = true
	}
	for _, want := range []string{"gateway.byok_fee_bps", "gateway.fx_usd_cny", "gateway.kill_switch"} {
		if !keysSeen[want] {
			t.Fatalf("the community surface should list %s; got %v", want, keysSeen)
		}
	}
	if keysSeen["cloud.signup_mode"] {
		t.Fatal("a Cloud-only key leaked into the Community surface")
	}

	put := func(reason string) error {
		body := communitystaffapi.SettingsBatch{Changes: []struct {
			Key   string `json:"key"`
			Value any    `json:"value"`
		}{{Key: "gateway.byok_fee_bps", Value: 750}}}
		if reason != "" {
			body.Reason = &reason
		}
		_, err := srv.CommunityPutSettings(actx, communitystaffapi.CommunityPutSettingsRequestObject{Body: &body})
		return err
	}
	var ce *httpx.CodeError
	if err := put(""); !errors.As(err, &ce) || ce.Code != errcode.CommonSettingsReasonRequired {
		t.Fatalf("a high-impact key without a reason should be refused: %v", err)
	}
	if err := put("fee review"); err != nil {
		t.Fatal(err)
	}
	var fee int64
	if found, err := set.Get(ctx, "gateway.byok_fee_bps", &fee); err != nil || !found || fee != 750 {
		t.Fatalf("fee after write = %d %v %v", fee, found, err)
	}
	var audited int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action = 'settings.update' AND target_id = 'gateway.byok_fee_bps'`).Scan(&audited); err != nil {
		t.Fatal(err)
	}
	if audited != 1 {
		t.Fatalf("audit rows = %d", audited)
	}
}
