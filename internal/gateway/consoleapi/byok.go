package gwconsoleapi

import (
	"context"
	"errors"
	"github.com/fairlb/fairlb/foundation/strutil"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/foundation/cursorpage"
	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/foundation/publicid"
	"github.com/fairlb/fairlb/internal/gateway/byok"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
)

// Bring-your-own-key, as a contract.
//
// The rules live in internal/gateway/byok (ADR-0180). What is left here is the
// org-scoped transaction (authorization above it, queries inside it), the DTO
// mapping, and one thing worth stating: **the verdict of a credential test
// travels in the body of a 200**, not in the HTTP status. "The credential is
// invalid" is a normal result of the test, not a failure of this API call, and
// a 4xx would leave the frontend unable to tell "the test ran and failed" from
// "the test could not run".

func byokHTTPError(err error) error {
	var invalid byok.InvalidError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &invalid):
		return httpx.ErrCodeDetail(errcode.CommonValidation, invalid.Message)
	case errors.Is(err, byok.ErrDuplicateName):
		return httpx.ErrCodeDetail(errcode.CommonValidation, "A key with this name already exists")
	case errors.Is(err, byok.ErrNotFound):
		return httpx.ErrCode(errcode.CommonNotFound)
	default:
		return err
	}
}

func byokKeyDTO(k byok.Key) OrgProviderKey {
	v := OrgProviderKey{
		Id:     publicid.Format(publicid.Key, pgtype.UUID{Bytes: k.ID, Valid: true}),
		Vendor: k.Vendor, VendorLabel: strutil.Ptr(k.VendorLabel), Name: k.Name,
		Status: OrgProviderKeyStatus(k.Status), SecretHint: k.SecretHint,
		AllowFallback: k.AllowFallback, CreatedAt: k.CreatedAt,
	}
	if k.BaseURL != "" {
		url := k.BaseURL
		v.BaseUrl = &url
	}
	if !k.LastVerifiedAt.IsZero() {
		at := k.LastVerifiedAt
		v.LastVerifiedAt = &at
	}
	return v
}

func byokVendorDTO(v byok.Vendor) OrgProviderVendor {
	return OrgProviderVendor{
		Vendor: v.Slug, Label: v.Label,
		BaseUrlHint:    strutil.Ptr(v.BaseURLHint),
		ModelIdExample: strutil.Ptr(v.ModelIDExample),
		KeyHint:        strutil.Ptr(v.KeyHint),
	}
}

func (s *Server) byok(q *gwdb.Queries) *byok.Service {
	return byok.NewService(q, s.probeClient)
}

func (s *Server) ListOrgProviderKeys(ctx context.Context, req ListOrgProviderKeysRequestObject) (ListOrgProviderKeysResponseObject, error) {
	page, err := httpx.ParseKeyPage(req.Params.Cursor, req.Params.Limit, byok.KeyCursorParts, 20, 100)
	if err != nil {
		return nil, err
	}
	var items []OrgProviderKey
	var vendors []OrgProviderVendor
	var next *string
	err = s.scopeAdminRead(ctx, req.OrgId, func(ctx context.Context, q *gwdb.Queries, org pgtype.UUID) error {
		svc := s.byok(q)
		keys, kErr := svc.Keys(ctx, org, page)
		if kErr != nil {
			return kErr
		}
		kept, more := cursorpage.Trim(keys, int(page.Limit))
		items = make([]OrgProviderKey, 0, len(kept))
		for _, k := range kept {
			items = append(items, byokKeyDTO(k))
		}
		if more {
			nc := byok.KeyCursor(kept[len(kept)-1])
			next = &nc
		}
		list, vErr := svc.Vendors(ctx)
		if vErr != nil {
			return vErr
		}
		vendors = make([]OrgProviderVendor, 0, len(list))
		for _, v := range list {
			vendors = append(vendors, byokVendorDTO(v))
		}
		return nil
	})
	if err != nil {
		return nil, byokHTTPError(err)
	}
	return ListOrgProviderKeys200JSONResponse{Items: items, Vendors: vendors, NextCursor: next}, nil
}

func (s *Server) CreateOrgProviderKey(ctx context.Context, req CreateOrgProviderKeyRequestObject) (CreateOrgProviderKeyResponseObject, error) {
	if req.Body == nil {
		return nil, httpx.ErrCode(errcode.CommonValidation)
	}
	in := byok.Create{
		Vendor: req.Body.Vendor, Name: req.Body.Name, Secret: req.Body.Secret,
		BaseURL:       req.Body.BaseUrl,
		AllowFallback: req.Body.AllowFallback != nil && *req.Body.AllowFallback,
	}
	var out OrgProviderKey
	err := s.scopeWrite(ctx, req.OrgId, func(ctx context.Context, q *gwdb.Queries, org pgtype.UUID) error {
		key, cErr := s.byok(q).Create(ctx, s.box, org, in)
		if cErr != nil {
			return cErr
		}
		out = byokKeyDTO(key)
		return nil
	})
	if err != nil {
		return nil, byokHTTPError(err)
	}
	return CreateOrgProviderKey201JSONResponse(out), nil
}

func (s *Server) DeleteOrgProviderKey(ctx context.Context, req DeleteOrgProviderKeyRequestObject) (DeleteOrgProviderKeyResponseObject, error) {
	keyID, err := publicid.Parse(publicid.Key, req.KeyId)
	if err != nil {
		return nil, httpx.ErrCode(errcode.CommonNotFound)
	}
	err = s.scopeWrite(ctx, req.OrgId, func(ctx context.Context, q *gwdb.Queries, org pgtype.UUID) error {
		return s.byok(q).Delete(ctx, org, keyID)
	})
	if err != nil {
		return nil, byokHTTPError(err)
	}
	return DeleteOrgProviderKey204Response{}, nil
}

func (s *Server) TestOrgProviderKey(ctx context.Context, req TestOrgProviderKeyRequestObject) (TestOrgProviderKeyResponseObject, error) {
	if req.Body == nil {
		return nil, httpx.ErrCodeDetail(errcode.CommonValidation, "upstream_model is required")
	}
	keyID, err := publicid.Parse(publicid.Key, req.KeyId)
	if err != nil {
		return nil, httpx.ErrCode(errcode.CommonNotFound)
	}
	var res byok.TestResult
	err = s.scopeWrite(ctx, req.OrgId, func(ctx context.Context, q *gwdb.Queries, org pgtype.UUID) error {
		var tErr error
		res, tErr = s.byok(q).Test(ctx, s.box, org, keyID, req.Body.UpstreamModel)
		return tErr
	})
	if err != nil {
		return nil, byokHTTPError(err)
	}
	out := OrgProviderKeyTestResult{
		CheckedAt: res.CheckedAt, Ok: res.Ok,
		LatencyMs: res.LatencyMs, StatusCode: res.StatusCode,
	}
	if res.Message != "" {
		out.Message = strutil.Ptr(res.Message)
	}
	return TestOrgProviderKey200JSONResponse(out), nil
}
