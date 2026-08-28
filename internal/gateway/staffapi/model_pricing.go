package gwstaffapi

import (
	"context"

	"github.com/google/uuid"

	"github.com/fairlb/fairlb/foundation/httpx"
)

func (s *Server) GetGatewayModelPricing(ctx context.Context, req GetGatewayModelPricingRequestObject) (GetGatewayModelPricingResponseObject, error) {
	resource, etag, err := s.pricingAdmin.GetModelPricing(ctx, uuid.UUID(req.ModelId))
	if err != nil {
		return nil, pricingHTTPError(err)
	}
	return GetGatewayModelPricing200JSONResponse{
		Body: resource, Headers: GetGatewayModelPricing200ResponseHeaders{ETag: etagHeader(etag)},
	}, nil
}

func (s *Server) SaveGatewayModelPricing(ctx context.Context, req SaveGatewayModelPricingRequestObject) (SaveGatewayModelPricingResponseObject, error) {
	if err := validateIfMatch(&req.Params.IfMatch); err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, pricingHTTPError(ErrPricingInvalid)
	}
	// Writing a price takes the highest privilege. Once saving is publishing,
	// there is no "it was only a draft" to fall back on, so the bar for who
	// may write goes up with it.
	actor, err := httpx.RequireSuperadmin(ctx)
	if err != nil {
		return nil, err
	}
	resource, etag, err := s.pricingAdmin.SaveModelPricing(
		ctx, uuid.UUID(req.ModelId), *req.Body, req.Params.IfMatch, actor,
	)
	if err != nil {
		return nil, pricingHTTPError(err)
	}
	return SaveGatewayModelPricing200JSONResponse{
		Body: resource, Headers: SaveGatewayModelPricing200ResponseHeaders{ETag: etagHeader(etag)},
	}, nil
}
