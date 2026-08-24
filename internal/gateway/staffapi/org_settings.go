package gwstaffapi

import (
	"context"
	"errors"
	"github.com/fairlb/fairlb/foundation/strutil"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/foundation/drivers/cache"
	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/foundation/publicid"
	"github.com/fairlb/fairlb/internal/gateway/proxy"
	"github.com/fairlb/fairlb/internal/gateway/tiers"
)

// An organization's admission settings, as a contract.
//
// The rules live in internal/gateway/tiers (ADR-0179). What is left here is
// parsing the public id, requiring a reason, mapping refusals to status codes,
// and naming the cache key — that last one because the key's shape belongs to
// the data plane, not to admission.

func orgUUIDOf(id OrgId) (pgtype.UUID, error) {
	org, err := publicid.Parse(publicid.Org, string(id))
	if err != nil {
		return pgtype.UUID{}, httpx.ErrCodeDetail(errcode.CommonValidation, "Invalid org_id")
	}
	return org, nil
}

// keyCacheDeleter names the data-plane cache key. It is the one thing about
// invalidation that is not admission's business: the key's shape is the proxy's.
type keyCacheDeleter struct{ cache cache.Store }

func (d keyCacheDeleter) InvalidateKeyHash(ctx context.Context, hash string) error {
	return d.cache.Delete(ctx, proxy.KeyCacheKey(hash))
}

func (s *Server) keyCache() tiers.KeyCacheInvalidator {
	if s.cache == nil {
		return nil
	}
	return keyCacheDeleter{cache: s.cache}
}

func orgSettingsHTTPError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, tiers.ErrNotFound):
		return httpx.ErrCodeDetail(errcode.CommonValidation, "Access tier not found")
	case errors.Is(err, tiers.ErrTierDisabled):
		return httpx.ErrCodeDetail(errcode.CommonConflict,
			"That tier is disabled and cannot be assigned; the data plane "+
				"refuses requests on a disabled tier")
	case errors.Is(err, tiers.ErrOrgNotFound):
		return httpx.ErrCodeDetail(errcode.CommonNotFound, "Organization not found")
	default:
		// ErrNoDefaultTier lands here on purpose: it is a server fault, and a
		// normal-looking response with no tier would make the operator page
		// display a tier that does not exist.
		return err
	}
}

func orgSettingsDTO(s tiers.OrgSettings) OrgGatewaySettings {
	out := OrgGatewaySettings{
		TierId: s.TierID, TierSlug: s.TierSlug, TierName: strutil.Ptr(s.TierName),
		TierStatus:         OrgGatewaySettingsTierStatus(s.TierStatus),
		TierAllowAllModels: s.TierAllowAllModels,
		TierExplicit:       s.TierExplicit, RowExists: s.RowExists,
	}
	if s.RateLimitRPM != nil {
		rpm := int(*s.RateLimitRPM)
		out.RateLimitRpm = &rpm
	}
	if s.RateLimitTPM != nil {
		tpm := int(*s.RateLimitTPM)
		out.RateLimitTpm = &tpm
	}
	return out
}

func (s *Server) GetOrgGatewaySettings(ctx context.Context, req GetOrgGatewaySettingsRequestObject) (GetOrgGatewaySettingsResponseObject, error) {
	org, err := orgUUIDOf(req.OrgId)
	if err != nil {
		return nil, err
	}
	settings, err := s.tiers.OrgSettings(ctx, org)
	if err != nil {
		return nil, orgSettingsHTTPError(err)
	}
	return GetOrgGatewaySettings200JSONResponse(orgSettingsDTO(settings)), nil
}

// PutOrgGatewaySettings replaces this organization's tier and rate ceilings as
// one row.
func (s *Server) PutOrgGatewaySettings(ctx context.Context, req PutOrgGatewaySettingsRequestObject) (PutOrgGatewaySettingsResponseObject, error) {
	org, err := orgUUIDOf(req.OrgId)
	if err != nil {
		return nil, err
	}
	in := req.Body
	if in == nil {
		return nil, httpx.ErrCodeDetail(errcode.CommonValidation, "A request body is required")
	}
	// The reason is a contract requirement, not a rule about admission: the
	// domain stores no reason for this write, the audit middleware records it.
	if strings.TrimSpace(in.Reason) == "" {
		return nil, httpx.ErrCodeDetail(errcode.CommonValidation, "A reason is required")
	}
	write := tiers.OrgSettingsWrite{TierID: in.TierId}
	if in.RateLimitRpm != nil {
		rpm := int32(*in.RateLimitRpm) //nolint:gosec // the spec bounds this to a positive integer and the column CHECKs > 0
		write.RateLimitRPM = &rpm
	}
	if in.RateLimitTpm != nil {
		tpm := int32(*in.RateLimitTpm) //nolint:gosec // the spec bounds this to a positive integer and the column CHECKs > 0
		write.RateLimitTPM = &tpm
	}
	settings, err := s.tiers.PutOrgSettings(ctx, org, write, s.keyCache())
	if err != nil {
		return nil, orgSettingsHTTPError(err)
	}
	return PutOrgGatewaySettings200JSONResponse(orgSettingsDTO(settings)), nil
}
