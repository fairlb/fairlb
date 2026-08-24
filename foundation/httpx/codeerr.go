package httpx

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/foundation/errcode"
)

// CodeError lets a handler return a registered error code in the shape of an
// error. OAPIResponseError recognizes it and renders the problem document from
// the registry; anything it does not recognize still falls back to 500.
type CodeError struct {
	Code   string
	Detail string
	// RetryAfter, when greater than zero, is emitted as a Retry-After header
	// in seconds (rate limiting, temporary lockouts).
	RetryAfter int
}

func (e *CodeError) Error() string {
	if e.Detail == "" {
		return e.Code
	}
	return e.Code + ": " + e.Detail
}

// ErrCode builds a CodeError carrying only the code.
func ErrCode(code string) *CodeError { return &CodeError{Code: code} }

// ErrCodeDetail builds a CodeError with a client-visible detail.
func ErrCodeDetail(code, detail string) *CodeError { return &CodeError{Code: code, Detail: detail} }

// RequireUser returns the authenticated user (internal uuid string); an
// anonymous subject yields a 401 CodeError.
func RequireUser(ctx context.Context) (string, error) {
	p := PrincipalFrom(ctx)
	if p.Subject == "" {
		return "", ErrCode(errcode.CommonUnauthenticated)
	}
	return p.Subject, nil
}

// RequireUserID is RequireUser with the subject parsed into the id type every
// caller immediately needs.
//
// It lives here rather than beside whichever package performed authentication,
// because it reads nothing but the Principal, and Principal is this package's
// type. Two products had a byte-identical private copy each; the one in the
// gateway carried a comment explaining that the copy was cheaper than importing
// the login package -- true, and it stayed true, because the readers never
// belonged to the login package in the first place.
func RequireUserID(ctx context.Context) (pgtype.UUID, error) {
	subject, err := RequireUser(ctx)
	if err != nil {
		return pgtype.UUID{}, err
	}
	var id pgtype.UUID
	if err := id.Scan(subject); err != nil {
		return pgtype.UUID{}, err
	}
	return id, nil
}

// RequireSuperadmin is RequireUserID plus the highest admin-plane privilege,
// answering 403 when the role is missing. Which principals hold the role is
// decided by the authenticator at the assembly point, so this invariant is one
// piece of code no matter how identities are issued.
func RequireSuperadmin(ctx context.Context) (pgtype.UUID, error) {
	id, err := RequireUserID(ctx)
	if err != nil {
		return pgtype.UUID{}, err
	}
	// The literal, not a constant: `staff_users.role` carries a CHECK
	// constraint naming this vocabulary, so a Go constant would be a second
	// declaration of the same fact rather than the single source it looks like.
	if PrincipalFrom(ctx).Role != "superadmin" {
		return pgtype.UUID{}, ErrCode(errcode.CommonForbidden)
	}
	return id, nil
}
