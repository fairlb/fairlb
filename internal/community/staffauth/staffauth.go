// Package staffauth is the administrator identity domain.
//
// # The shape it has to fit
//
// It implements the standard authenticator contract, reads the staff_sessions
// table, and produces the standard principal -- so the admin-facing middleware
// and endpoints in the gateway need no knowledge of it at all.
//
// What it deliberately does not have: mandatory TOTP, an IP allowlist,
// impersonation, or staff account governance (creating, disabling, resetting
// other administrators). Those belong to a multi-tenant operations desk. An
// administrator of a single self-hosted deployment, working on their own
// installation, has no use for "operator A steps into organization B's session to
// investigate", and should not be forced to enrol a TOTP device just to log in.
//
// # The role is always superadmin
//
// There is a single administrator identity here, with no operator/superadmin
// split. The stored role is superadmin, so the gateway's superadmin check
// always passes. The decision logic lives in exactly one place; only the answer
// to "who fills in the role" differs by deployment. That keeps an invariant
// like "writing prices requires the highest privilege" from being changed on
// one side and forgotten on the other.
package staffauth

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fairlb/fairlb/access/staff"
	"github.com/fairlb/fairlb/foundation/crypto"
	"github.com/fairlb/fairlb/foundation/db"
	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/foundation/publicid"
)

const (
	// sessionTTL: session lifetime is a security parameter and is not relaxed
	// for any particular deployment shape.
	sessionTTL = 12 * time.Hour
	// touchInterval: no refresh within this window, so not every request
	// writes to the database.
	touchInterval = 5 * time.Minute
	// SessionCookie is the name used when the `__Host-` contract can actually
	// be honoured: same origin, Secure, no Domain attribute.
	SessionCookie = "__Host-flb_staff"
	// SessionCookieInsecure is the name used when it cannot -- see
	// SessionCookieName.
	SessionCookieInsecure = "flb_staff"
)

// SessionCookieName picks the cookie name according to whether this response
// can carry Secure.
//
// `__Host-` is a contract (RFC 6265bis): the cookie must be Secure, Path=/, and
// carry no Domain. A cookie under that name without Secure is discarded
// outright by browsers and by curl -- while the login still returns 204. The
// symptom becomes "logged in, then immediately not", which is the hardest kind
// to diagnose: the status code is right and the header was sent, the recipient
// simply refused to store it as the specification requires.
//
// This is not hypothetical: starting a container with the documented defaults
// and no TLS leaves curl's cookie jar empty and the very next authenticated
// call returning 401.
//
// Why not simply always set Secure: plaintext deployment is a common
// self-hosting shape (a LAN address such as http://192.168.x.x:8080), where a
// Secure cookie cannot be sent at all and login fails just as thoroughly. So:
// use the prefix when it can be honoured, and a name that does not lie when it
// cannot. A name must not promise a precondition that is not currently met.
func SessionCookieName(secure bool) string {
	if secure {
		return SessionCookie
	}
	return SessionCookieInsecure
}

// Service is the administrator identity surface: log in, log out, resolve a
// session.
type Service struct {
	store *staff.Store
}

func New(pool *pgxpool.Pool) *Service {
	return &Service{store: staff.New(pool)}
}

// Login verifies the email and password, issues a session, and returns the
// plaintext token exactly once.
//
// A wrong password and a nonexistent account return the same error:
// distinguishing them hands an enumeration attack a channel. The comparison is
// also constant-time in the sense that matters here -- the password hash is
// computed even for an account that does not exist, because otherwise the
// response time itself becomes that channel.
func (s *Service) Login(ctx context.Context, email, password string) (string, error) {
	u, err := s.store.ByEmail(ctx, email)
	if err != nil && !db.IsNoRows(err) {
		return "", err
	}
	hash := u.PasswordHash
	if db.IsNoRows(err) || u.Status != "active" {
		hash = crypto.DummyPasswordHash()
	}
	ok, vErr := crypto.VerifyPassword(password, hash)
	if vErr != nil || !ok || db.IsNoRows(err) || u.Status != "active" {
		return "", httpx.ErrCode(errcode.CommonUnauthenticated)
	}

	token, tErr := crypto.NewToken(32)
	if tErr != nil {
		return "", tErr
	}
	if _, err := s.store.CreateSession(ctx, staff.CreateSession{
		StaffUserID: u.ID,
		TokenHash:   crypto.HashToken(token),
		ExpiresAt:   pgtype.Timestamptz{Time: time.Now().Add(sessionTTL), Valid: true},
	}); err != nil {
		return "", err
	}
	return token, nil
}

// Logout revokes one session. It is idempotent: an already revoked or
// nonexistent session is not an error.
func (s *Service) Logout(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	return s.store.RevokeSessionByTokenHash(ctx, crypto.HashToken(token))
}

// LogoutBySubject revokes every session belonging to an administrator.
//
// A strict handler has no access to the original request (its signature carries
// only a context), so logout revokes by subject rather than by token. The side
// effect is that the account's other sessions end too -- which for a single
// self-hosted deployment is what you want: if the administrator logged in
// somewhere else and forgot, logging out should really log them out.
func (s *Service) LogoutBySubject(ctx context.Context, subject string) error {
	var id pgtype.UUID
	if err := id.Scan(subject); err != nil {
		return nil // The subject is not a uuid, so there is no session to revoke.
	}
	return s.store.RevokeSessions(ctx, id)
}

// Authenticator resolves an administrator session into a principal.
type Authenticator struct {
	svc        *Service
	cookieName string
}

func NewAuthenticator(svc *Service, secure bool) *Authenticator {
	return &Authenticator{svc: svc, cookieName: SessionCookieName(secure)}
}

func (a *Authenticator) Authenticate(r *http.Request) (httpx.Principal, error) {
	// An anonymous principal rather than an error: the public login endpoint
	// has to be reachable, and refusing is the endpoint-side staff check's
	// job. Otherwise the login page itself would be unreachable.
	anon := httpx.Principal{Scope: "admin"}
	c, err := r.Cookie(a.cookieName)
	if err != nil || c.Value == "" {
		return anon, nil
	}
	ctx := r.Context()
	row, err := a.svc.store.SessionForAuth(ctx, crypto.HashToken(c.Value))
	if db.IsNoRows(err) {
		return anon, nil
	}
	if err != nil {
		slog.ErrorContext(ctx, "looking up the administrator session failed", "error", err)
		return anon, nil
	}
	if row.StaffStatus != "active" {
		return anon, nil
	}
	if time.Since(row.LastSeenAt.Time) > touchInterval {
		if err := a.svc.store.TouchSession(ctx, row.ID,
			pgtype.Timestamptz{Time: time.Now().Add(sessionTTL), Valid: true}); err != nil {
			slog.WarnContext(ctx, "refreshing the administrator session failed", "error", err)
		}
	}
	// The role comes from the stored row. Account creation writes superadmin;
	// hard-coding it here instead would turn "the row says operator but the
	// code treats it as superadmin" into an invisible privilege escalation.
	return httpx.Principal{
		Scope: "admin", Subject: uuidStr(row.StaffUserID), Role: row.StaffRole,
	}, nil
}

var _ httpx.Authenticator = (*Authenticator)(nil)

var uuidStr = publicid.UUIDString
