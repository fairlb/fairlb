package gwstaffapi

import (
	"context"

	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
)

func (s *Server) GetGatewayOrgPricingPlan(ctx context.Context, req GetGatewayOrgPricingPlanRequestObject) (GetGatewayOrgPricingPlanResponseObject, error) {
	if _, err := orgUUIDOf(req.OrgId); err != nil {
		return nil, err
	}
	assignment, etag, err := s.pricingAdmin.GetOrgPricingPlan(ctx, string(req.OrgId))
	if err != nil {
		return nil, pricingHTTPError(err)
	}
	return GetGatewayOrgPricingPlan200JSONResponse{
		Body: assignment, Headers: GetGatewayOrgPricingPlan200ResponseHeaders{ETag: etagHeader(etag)},
	}, nil
}

func (s *Server) AssignGatewayOrgPricingPlan(ctx context.Context, req AssignGatewayOrgPricingPlanRequestObject) (AssignGatewayOrgPricingPlanResponseObject, error) {
	if _, err := orgUUIDOf(req.OrgId); err != nil {
		return nil, err
	}
	if err := validateIfMatch(req.Params.IfMatch); err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, httpx.ErrCodeDetail(errcode.CommonValidation, "A request body is required")
	}
	if err := validatePricingReason(&req.Body.Reason); err != nil {
		return nil, err
	}
	actor, err := httpx.RequireSuperadmin(ctx)
	if err != nil {
		return nil, err
	}
	assignment, etag, err := s.pricingAdmin.AssignOrgPricingPlan(
		ctx, string(req.OrgId), *req.Body, req.Params.IfMatch, actor,
	)
	if err != nil {
		return nil, pricingHTTPError(err)
	}
	// An assignment that takes effect now must invalidate the identity cache.
	// A future-dated one deliberately does not: clearing the cache early would
	// only refill it with the same old snapshot, and the activation worker
	// clears it precisely when the assignment becomes current. That also
	// covers an explicit effective_at that crosses "now" while the transaction
	// is running.
	return AssignGatewayOrgPricingPlan200JSONResponse{
		Body:    assignment,
		Headers: AssignGatewayOrgPricingPlan200ResponseHeaders{ETag: etagHeader(etag)},
	}, nil
}
