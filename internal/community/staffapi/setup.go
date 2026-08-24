package staffapi

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"

	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/internal/community/bootstrap"
)

// SetupConfig carries what the first-run endpoints need beyond the database.
type SetupConfig struct {
	// Version is reported by GET /meta.
	Version string
	// Token, when non-empty, must be presented to POST /setup.
	Token string
}

// CommunityMeta reports whether this instance still needs an administrator.
//
// Anonymous by necessity: the sign-in page calls it to decide which screen to
// render, and that happens before anyone can be signed in.
func (s *Server) CommunityMeta(ctx context.Context, _ CommunityMetaRequestObject) (CommunityMetaResponseObject, error) {
	exists, err := bootstrap.AdminExists(ctx, s.pool)
	if err != nil {
		return nil, err
	}
	state := Available
	if exists {
		state = Complete
	}
	return CommunityMeta200JSONResponse{
		Version:            s.setup.Version,
		SetupState:         state,
		SetupRequiresToken: s.setup.Token != "",
	}, nil
}

// CommunitySetup creates the first administrator and signs them in.
func (s *Server) CommunitySetup(ctx context.Context, req CommunitySetupRequestObject) (CommunitySetupResponseObject, error) {
	if req.Body == nil {
		return nil, httpx.ErrCodeDetail(errcode.CommonValidation, "request body is required")
	}
	if s.setup.Token != "" {
		given := ""
		if req.Body.Token != nil {
			given = *req.Body.Token
		}
		// Constant-time: a token compared with == leaks its prefix through
		// response timing, and this endpoint is anonymous and unlimited in how
		// often it may be guessed at, short of the rate limiter.
		if subtle.ConstantTimeCompare([]byte(given), []byte(s.setup.Token)) != 1 {
			return nil, httpx.ErrCodeDetail(errcode.CommonForbidden,
				"setup token does not match")
		}
	}

	err := bootstrap.CreateFirstAdmin(ctx, s.pool, string(req.Body.Email), req.Body.Password, "")
	switch {
	case errors.Is(err, bootstrap.ErrPasswordTooShort):
		return nil, httpx.ErrCodeDetail(errcode.CommonValidation, err.Error())
	case errors.Is(err, bootstrap.ErrAlreadyConfigured):
		// 409 rather than a redirect to sign-in: whoever sent this believes
		// they are creating the instance's owner, and they are not.
		return nil, httpx.ErrCodeDetail(errcode.CommonConflict,
			"this instance already has an administrator")
	case err != nil:
		return nil, err
	}

	token, err := s.svc.Login(ctx, string(req.Body.Email), req.Body.Password)
	if err != nil {
		// The account exists at this point, so the operator is not locked out —
		// the sign-in page works. Report it rather than pretending setup failed.
		return nil, err
	}
	return sessionCookie{token: token, secure: s.secure}, nil
}

// VisitCommunitySetupResponse lets the same cookie-setting response type serve
// both sign-in and setup, so the cookie's attributes are defined once.
func (r sessionCookie) VisitCommunitySetupResponse(w http.ResponseWriter) error {
	return r.VisitCommunityLoginResponse(w)
}
