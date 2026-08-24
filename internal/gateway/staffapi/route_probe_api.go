package gwstaffapi

import (
	"context"

	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/internal/gateway/routeprobe"
)

// The operator's two writes on a route's capability record. Both are thin: the
// rule that the endpoint must belong to a protocol the provider speaks, and
// what each status means, live in catalogadmin.

func (s *Server) ProbeGatewayRoute(ctx context.Context, req ProbeGatewayRouteRequestObject) (ProbeGatewayRouteResponseObject, error) {
	var endpoints []string
	if req.Body != nil && req.Body.Endpoints != nil {
		endpoints = *req.Body.Endpoints
	}
	if err := s.catalogAdmin.ProbeRoute(ctx, req.ModelId, req.RouteId, endpoints); err != nil {
		return nil, routeHTTPError(err)
	}
	return ProbeGatewayRoute202Response{}, nil
}

func (s *Server) SetGatewayRouteProbe(ctx context.Context, req SetGatewayRouteProbeRequestObject) (SetGatewayRouteProbeResponseObject, error) {
	if req.Body == nil {
		return nil, httpx.ErrCodeDetail(errcode.CommonValidation, "A request body is required")
	}
	v, err := s.catalogAdmin.OverrideRouteProbe(ctx, req.ModelId, req.RouteId, req.Endpoint, string(req.Body.Status))
	if err != nil {
		return nil, routeHTTPError(err)
	}
	return SetGatewayRouteProbe200JSONResponse(probeVerdictsList([]routeprobe.Verdict{v})[0]), nil
}
