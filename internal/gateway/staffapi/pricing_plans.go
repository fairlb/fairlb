package gwstaffapi

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/internal/gateway/pricing"
)

func (s *Server) ListGatewayPricingPlans(ctx context.Context, req ListGatewayPricingPlansRequestObject) (ListGatewayPricingPlansResponseObject, error) {
	page, err := httpx.ParseKeyPage(
		req.Params.Cursor, req.Params.Limit, pricing.PlanCursorParts, 50, 200)
	if err != nil {
		return nil, err
	}
	plans, next, err := s.pricingAdmin.ListPricingPlans(ctx, page, derefOr(req.Params.Q, ""))
	if err != nil {
		return nil, pricingHTTPError(err)
	}
	if plans == nil {
		plans = []PricingPlan{}
	}
	resp := ListGatewayPricingPlans200JSONResponse{Items: plans}
	if next != "" {
		resp.NextCursor = &next
	}
	return resp, nil
}

func (s *Server) CreateGatewayPricingPlan(ctx context.Context, req CreateGatewayPricingPlanRequestObject) (CreateGatewayPricingPlanResponseObject, error) {
	if req.Body == nil {
		return nil, httpx.ErrCodeDetail(errcode.CommonValidation, "A request body is required")
	}
	req.Body.Slug = strings.TrimSpace(req.Body.Slug)
	req.Body.Name = strings.TrimSpace(req.Body.Name)
	if req.Body.Slug == "" || req.Body.Name == "" {
		return nil, httpx.ErrCodeDetail(errcode.CommonValidation, "slug and name are required")
	}
	if err := validateAdjustment(req.Body.DefaultAdjustment); err != nil {
		return nil, err
	}
	actor, err := httpx.RequireUserID(ctx)
	if err != nil {
		return nil, err
	}
	plan, etag, err := s.pricingAdmin.CreatePricingPlan(ctx, *req.Body, actor)
	if err != nil {
		return nil, pricingHTTPError(err)
	}
	return CreateGatewayPricingPlan201JSONResponse{
		Body: plan, Headers: CreateGatewayPricingPlan201ResponseHeaders{ETag: etagHeader(etag)},
	}, nil
}

func (s *Server) GetGatewayPricingPlan(ctx context.Context, req GetGatewayPricingPlanRequestObject) (GetGatewayPricingPlanResponseObject, error) {
	plan, etag, err := s.pricingAdmin.GetPricingPlan(ctx, uuid.UUID(req.PricingPlanId))
	if err != nil {
		return nil, pricingHTTPError(err)
	}
	return GetGatewayPricingPlan200JSONResponse{
		Body: plan, Headers: GetGatewayPricingPlan200ResponseHeaders{ETag: etagHeader(etag)},
	}, nil
}

func (s *Server) UpdateGatewayPricingPlan(ctx context.Context, req UpdateGatewayPricingPlanRequestObject) (UpdateGatewayPricingPlanResponseObject, error) {
	if err := validateIfMatch(&req.Params.IfMatch); err != nil {
		return nil, err
	}
	if req.Body == nil || (req.Body.Name == nil && req.Body.Description == nil && req.Body.Status == nil) {
		return nil, httpx.ErrCodeDetail(errcode.CommonValidation, "Provide at least one field to update")
	}
	if req.Body.Name != nil {
		name := strings.TrimSpace(*req.Body.Name)
		if name == "" {
			return nil, httpx.ErrCodeDetail(errcode.CommonValidation, "name must not be empty")
		}
		req.Body.Name = &name
	}
	actor, err := httpx.RequireUserID(ctx)
	if err != nil {
		return nil, err
	}
	// Enabling or disabling a plan can leave a whole set of organizations with no
	// effective price, so it takes the same privilege as publishing one.
	if req.Body.Status != nil {
		actor, err = httpx.RequireSuperadmin(ctx)
		if err != nil {
			return nil, err
		}
	}
	plan, etag, err := s.pricingAdmin.UpdatePricingPlan(
		ctx, uuid.UUID(req.PricingPlanId), *req.Body, req.Params.IfMatch, actor,
	)
	if err != nil {
		return nil, pricingHTTPError(err)
	}
	return UpdateGatewayPricingPlan200JSONResponse{
		Body: plan, Headers: UpdateGatewayPricingPlan200ResponseHeaders{ETag: etagHeader(etag)},
	}, nil
}

func (s *Server) DeleteGatewayPricingPlan(ctx context.Context, req DeleteGatewayPricingPlanRequestObject) (DeleteGatewayPricingPlanResponseObject, error) {
	if err := validateIfMatch(&req.Params.IfMatch); err != nil {
		return nil, err
	}
	actor, err := httpx.RequireSuperadmin(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.pricingAdmin.DeletePricingPlan(ctx, uuid.UUID(req.PricingPlanId), req.Params.IfMatch, actor); err != nil {
		return nil, pricingHTTPError(err)
	}
	return DeleteGatewayPricingPlan204Response{}, nil
}

func (s *Server) CopyGatewayPricingPlan(ctx context.Context, req CopyGatewayPricingPlanRequestObject) (CopyGatewayPricingPlanResponseObject, error) {
	if req.Body == nil {
		return nil, httpx.ErrCodeDetail(errcode.CommonValidation, "A request body is required")
	}
	req.Body.Slug = strings.TrimSpace(req.Body.Slug)
	req.Body.Name = strings.TrimSpace(req.Body.Name)
	if req.Body.Slug == "" || req.Body.Name == "" {
		return nil, httpx.ErrCodeDetail(errcode.CommonValidation, "slug and name are required")
	}
	actor, err := httpx.RequireUserID(ctx)
	if err != nil {
		return nil, err
	}
	plan, etag, err := s.pricingAdmin.CopyPricingPlan(ctx, uuid.UUID(req.PricingPlanId), *req.Body, actor)
	if err != nil {
		return nil, pricingHTTPError(err)
	}
	return CopyGatewayPricingPlan201JSONResponse{
		Body: plan, Headers: CopyGatewayPricingPlan201ResponseHeaders{ETag: etagHeader(etag)},
	}, nil
}

func (s *Server) ListGatewayPricingPlanModelOverrides(ctx context.Context, req ListGatewayPricingPlanModelOverridesRequestObject) (ListGatewayPricingPlanModelOverridesResponseObject, error) {
	items, etag, err := s.pricingAdmin.ListPricingPlanModelOverrides(ctx, uuid.UUID(req.PricingPlanId))
	if err != nil {
		return nil, pricingHTTPError(err)
	}
	if items == nil {
		items = []PricingPlanModelOverride{}
	}
	resp := ListGatewayPricingPlanModelOverrides200JSONResponse{
		Headers: ListGatewayPricingPlanModelOverrides200ResponseHeaders{ETag: etagHeader(etag)},
	}
	resp.Body.Items = items
	return resp, nil
}

func (s *Server) ReplaceGatewayPricingPlanModelOverrides(ctx context.Context, req ReplaceGatewayPricingPlanModelOverridesRequestObject) (ReplaceGatewayPricingPlanModelOverridesResponseObject, error) {
	if err := validateIfMatch(&req.Params.IfMatch); err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, httpx.ErrCodeDetail(errcode.CommonValidation, "A request body is required")
	}
	if duplicateModelOverride(req.Body.Overrides) {
		return nil, httpx.ErrCodeDetail(errcode.CommonValidation, "A model cannot be overridden twice in the same request")
	}
	for _, item := range req.Body.Overrides {
		if err := validateAdjustment(item.Adjustment); err != nil {
			return nil, err
		}
	}
	actor, err := httpx.RequireUserID(ctx)
	if err != nil {
		return nil, err
	}
	items, etag, err := s.pricingAdmin.ReplacePricingPlanModelOverrides(
		ctx, uuid.UUID(req.PricingPlanId), req.Body.Overrides, req.Params.IfMatch, actor,
	)
	if err != nil {
		return nil, pricingHTTPError(err)
	}
	if items == nil {
		items = []PricingPlanModelOverride{}
	}
	resp := ReplaceGatewayPricingPlanModelOverrides200JSONResponse{
		Headers: ReplaceGatewayPricingPlanModelOverrides200ResponseHeaders{ETag: etagHeader(etag)},
	}
	resp.Body.Items = items
	return resp, nil
}
