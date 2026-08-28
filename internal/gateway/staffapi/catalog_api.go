package gwstaffapi

import (
	"context"
	"github.com/fairlb/fairlb/foundation/strutil"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"errors"
	"github.com/fairlb/fairlb/audit"
	"github.com/fairlb/fairlb/foundation/cursorpage"
	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/foundation/publicid"
	"github.com/fairlb/fairlb/internal/gateway/catalogadmin"
	"github.com/fairlb/fairlb/internal/gateway/routeprobe"
)

// Catalog write endpoints. The read side lives in server.go; every write goes
// through this file.
//
// Three rules run through all of it:
//
//   - Every write must call InvalidateAll. If the catalog cache is not
//     invalidated, a config change has no effect, and "I changed it and nothing
//     happened" is one of the most expensive kinds of bug to chase.
//   - A credential appears in clear text exactly once, in the create request.
//     It is encrypted on the way into the database and no endpoint can read it
//     back afterwards.
//   - Writes to a sub-resource always carry the parent id in the WHERE clause
//     (a route carries model_id, a key carries provider_id), so a tampered path
//     parameter fails silently instead of modifying somebody else's resource.

// ===== Provider credentials =====

func providerKeyDTO(k catalogadmin.ProviderKey) GatewayProviderKey {
	out := GatewayProviderKey{
		Id: k.ID, ProviderId: k.ProviderID,
		Name: strutil.Ptr(k.Name), Status: GatewayProviderKeyStatus(k.Status),
		SecretHint: k.SecretHint, LastError: strutil.Ptr(k.LastError),
	}
	if !k.LastVerifiedAt.IsZero() {
		at := k.LastVerifiedAt
		out.LastVerifiedAt = &at
	}
	if !k.CooldownUntil.IsZero() {
		at := k.CooldownUntil
		out.CooldownUntil = &at
	}
	return out
}

func (s *Server) ListGatewayProviderKeys(ctx context.Context, req ListGatewayProviderKeysRequestObject) (ListGatewayProviderKeysResponseObject, error) {
	page, err := httpx.ParseCursorPage(req.Params.Cursor, req.Params.Limit, 50, 200)
	if err != nil {
		return nil, err
	}
	keys, err := s.catalogAdmin.ProviderKeys(ctx, req.ProviderId, page)
	if err != nil {
		return nil, keyHTTPError(err)
	}
	// The verified count is over the whole set, so it is asked of the database
	// rather than derived from the page above (ADR-0188).
	verified, err := s.catalogAdmin.VerifiedProviderKeys(ctx, req.ProviderId)
	if err != nil {
		return nil, keyHTTPError(err)
	}
	kept, more := cursorpage.Trim(keys, int(page.Limit))
	data := make([]GatewayProviderKey, 0, len(kept))
	for _, k := range kept {
		data = append(data, providerKeyDTO(k))
	}
	resp := ListGatewayProviderKeys200JSONResponse{Items: data, VerifiedCount: verified}
	if more {
		nc := catalogadmin.ProviderKeyCursor(kept[len(kept)-1])
		resp.NextCursor = &nc
	}
	return resp, nil
}

// keyHTTPError maps the catalog writer's refusals for credential operations.
// Same table as routeHTTPError except for the not-found sentence, which names
// the thing the caller actually asked about.
func keyHTTPError(err error) error {
	if errors.Is(err, catalogadmin.ErrNotFound) {
		return httpx.ErrCodeDetail(errcode.CommonNotFound, "Key not found")
	}
	return routeHTTPError(err)
}

func (s *Server) UpdateGatewayProviderKey(ctx context.Context, req UpdateGatewayProviderKeyRequestObject) (UpdateGatewayProviderKeyResponseObject, error) {
	if req.Body == nil || (req.Body.Name == nil && req.Body.Status == nil) {
		return nil, httpx.ErrCodeDetail(errcode.CommonValidation, "Provide at least one of name or status")
	}
	patch := catalogadmin.KeyPatch{Name: req.Body.Name}
	if req.Body.Status != nil {
		status := string(*req.Body.Status)
		patch.Status = &status
	}
	key, err := s.catalogAdmin.UpdateProviderKey(ctx, req.ProviderId, req.KeyId, patch)
	if err != nil {
		return nil, keyHTTPError(err)
	}
	return UpdateGatewayProviderKey200JSONResponse(providerKeyDTO(key)), nil
}

func (s *Server) CreateGatewayProviderKey(ctx context.Context, req CreateGatewayProviderKeyRequestObject) (CreateGatewayProviderKeyResponseObject, error) {
	if req.Body == nil {
		return nil, httpx.ErrCodeDetail(errcode.CommonValidation, "secret is required")
	}
	key, err := s.catalogAdmin.CreateProviderKey(
		ctx, s.box, req.ProviderId, derefOr(req.Body.Name, ""), req.Body.Secret)
	if err != nil {
		return nil, keyHTTPError(err)
	}
	return CreateGatewayProviderKey201JSONResponse(providerKeyDTO(key)), nil
}

func (s *Server) DeleteGatewayProviderKey(ctx context.Context, req DeleteGatewayProviderKeyRequestObject) (DeleteGatewayProviderKeyResponseObject, error) {
	if err := s.catalogAdmin.DeleteProviderKey(ctx, req.ProviderId, req.KeyId); err != nil {
		return nil, keyHTTPError(err)
	}
	return DeleteGatewayProviderKey204Response{}, nil
}

// TestGatewayProvider sends one minimal request upstream using this provider's
// credential.
//
// Both success and failure return 200: a failed probe is the result of the
// test, not an error in the endpoint. Expressing it as 4xx/5xx would force every
// client to distinguish "the probe ran and did not pass" from "the probe
// endpoint itself is broken", and those two call for completely different
// responses.
func (s *Server) TestGatewayProvider(ctx context.Context, req TestGatewayProviderRequestObject) (TestGatewayProviderResponseObject, error) {
	if req.Body == nil {
		return nil, httpx.ErrCodeDetail(errcode.CommonValidation, "upstream_model is required")
	}
	in := catalogadmin.ConnectivityRequest{UpstreamModel: req.Body.UpstreamModel}
	if req.Body.Endpoint != nil {
		in.Endpoint = string(*req.Body.Endpoint)
	}
	if req.Body.KeyId != nil {
		in.KeyID = *req.Body.KeyId
	}
	// The full trace contains the credential in clear text, so it takes both
	// conditions: the deployment has to allow it (wired at the assembly point)
	// and the caller has to ask for it. Missing either one means "not asked
	// for", which is the path an untrusted deployment always takes.
	in.WantTrace = s.traceEnabled && req.Body.IncludeTrace != nil && *req.Body.IncludeTrace

	res, err := s.catalogAdmin.TestConnectivity(ctx, s.box, s.client(), req.ProviderId, in)
	if err != nil {
		if errors.Is(err, catalogadmin.ErrNotFound) {
			return nil, httpx.ErrCodeDetail(errcode.CommonNotFound, "Provider not found")
		}
		return nil, routeHTTPError(err)
	}

	// Capturing a trace writes an audit entry: this request handed out a
	// credential in clear text, which has to be attributable afterwards. It
	// stays here rather than in the domain because what is being recorded is a
	// property of *this request* -- who asked, over which request id.
	//
	// If the audit write fails, the trace is not sent. The feature stands on its
	// guard rails, and half of that guard is "there is a record". Failing to
	// record it falls back to the ordinary probe result, with every other field
	// returned as usual -- the probe has already been paid for.
	//
	// This deliberately does not mark the request as audited, which would
	// suppress the middleware's fallback entry. The fallback carries the client
	// IP and this one cannot: a strict handler has the context but not the
	// *http.Request. For a credential-disclosure event, losing the source IP is
	// worse than keeping two rows, so both are written.
	if res.Trace != nil {
		if err := s.auditTraceCapture(ctx, req.ProviderId, in.Endpoint, res.StatusCode); err != nil {
			res.Trace = nil
			res.Message = strings.TrimSpace(res.Message + " (trace discarded: the audit write failed)")
		}
	}
	// Recorded after the message may have been amended: a stored result naming
	// a message nobody can reproduce is worse than one that says nothing.
	if res.KeyID != (uuid.UUID{}) {
		s.catalogAdmin.RecordKeyVerification(ctx, res.KeyID, res.Message)
	}
	return TestGatewayProvider200JSONResponse(probeResultDTO(res)), nil
}

// auditTraceCapture records that a trace containing a plaintext credential was
// taken.
//
// What is recorded is that it happened, not what was in it. The trace itself is
// deliberately never stored: writing it to the audit table would leave a
// plaintext credential in a table that operators can query, export, and that is
// retained for years -- far more dangerous than returning it once in a
// response. Meta therefore carries only what is needed to locate the event.
func (s *Server) auditTraceCapture(ctx context.Context, providerID uuid.UUID, endpoint string, status *int) error {
	staffID, err := httpx.RequireUserID(ctx)
	if err != nil {
		// The middleware already rejected anonymous callers, so reaching this
		// means the context is not what it should be.
		return err
	}
	meta := map[string]any{"endpoint": endpoint}
	if status != nil {
		meta["status_code"] = *status
	}
	return audit.InsertTx(ctx, s.pool, audit.Entry{
		ActorType:  "staff",
		ActorID:    staffID,
		Action:     "gateway.provider.trace_captured",
		TargetType: "gateway_provider",
		TargetID:   publicid.UUIDString(uuidOf(providerID)),
		Meta:       meta,
		RequestID:  httpx.RequestIDFrom(ctx),
	})
}

func (s *Server) client() *http.Client {
	if s.httpClient != nil {
		return s.httpClient
	}
	return &http.Client{Timeout: routeprobe.Timeout, Transport: httpx.UpstreamTransport()}
}

func modelDTO(m catalogadmin.Model) GatewayModel {
	return GatewayModel{
		Id: m.ID, Slug: m.Slug, Enabled: m.Enabled,
		Visibility: GatewayModelVisibility(m.Visibility), DisplayName: strutil.Ptr(m.DisplayName),
		// Pricing is not reported here: it lives in model_pricing, and the
		// response to a create or update does not query it. Nor does it query
		// routes -- the verified endpoints, the protocols and the route count
		// come from the list endpoint.
		Endpoints: []string{}, Protocols: []string{},
		OutputModalities: m.OutputModalities,
	}
}

func (s *Server) CreateGatewayModel(ctx context.Context, req CreateGatewayModelRequestObject) (CreateGatewayModelResponseObject, error) {
	in := req.Body
	if in == nil || in.Slug == nil {
		return nil, httpx.ErrCodeDetail(errcode.CommonValidation, "slug is required")
	}
	create := catalogadmin.ModelCreate{
		Slug: *in.Slug, DisplayName: derefOr(in.DisplayName, ""),
		Visibility:       visibilityOr(in.Visibility, "public"),
		OutputModalities: derefModalities(in.OutputModalities),
		ContextWindow:    int32(derefInt(in.ContextWindow, 0)),
	}
	if in.MaxOutputTokens != nil {
		n := int32(*in.MaxOutputTokens)
		create.MaxOutputTokens = &n
	}
	model, err := s.catalogAdmin.CreateModel(ctx, create)
	if err != nil {
		return nil, routeHTTPError(err)
	}
	return CreateGatewayModel201JSONResponse(modelDTO(model)), nil
}

func (s *Server) UpdateGatewayModel(ctx context.Context, req UpdateGatewayModelRequestObject) (UpdateGatewayModelResponseObject, error) {
	in := req.Body
	if in == nil {
		return nil, httpx.ErrCodeDetail(errcode.CommonValidation, "A request body is required")
	}
	model, err := s.catalogAdmin.UpdateModel(ctx, req.ModelId, catalogadmin.ModelPatch{
		DisplayName: in.DisplayName, Enabled: in.Enabled,
		Visibility:       visibilityPtr(in.Visibility),
		OutputModalities: derefModalities(in.OutputModalities),
		ContextWindow:    int32PtrFromInt(in.ContextWindow),
		MaxOutputTokens:  int32PtrFromInt(in.MaxOutputTokens),
	})
	if err != nil {
		return nil, routeHTTPError(err)
	}
	return UpdateGatewayModel200JSONResponse(modelDTO(model)), nil
}

func routeDTO(r catalogadmin.Route) GatewayRoute {
	probes := probeVerdictsList(r.Verdicts)
	out := GatewayRoute{
		Id: r.ID, ModelId: r.ModelID, ModelSlug: r.ModelSlug, ProviderId: r.ProviderID,
		ProviderSlug: r.ProviderSlug, ProviderProtocols: r.ProviderProtocols,
		ProviderModelId: r.ProviderModelID, Priority: int(r.Priority),
		Weight: int(r.Weight), Enabled: r.Enabled,
	}
	if r.Verdicts != nil {
		out.Probes = &probes
	}
	if r.Headers != nil {
		h := r.Headers
		out.Headers = &h
	}
	if r.Quirks != nil {
		q := r.Quirks
		out.Quirks = &q
	}
	if r.ContextWindow != nil {
		n := int(*r.ContextWindow)
		out.ContextWindow = &n
	}
	if r.MaxOutputTokens != nil {
		n := int(*r.MaxOutputTokens)
		out.MaxOutputTokens = &n
	}
	if r.MaxImages != nil {
		n := int(*r.MaxImages)
		out.MaxImages = &n
	}
	out.VideoEnvelope = videoEnvelopeDTO(r.VideoEnvelope)
	return out
}

func routesDTO(routes []catalogadmin.Route) []GatewayRoute {
	out := make([]GatewayRoute, 0, len(routes))
	for _, r := range routes {
		out = append(out, routeDTO(r))
	}
	return out
}

// routeHTTPError maps the catalog writer's refusals.
func routeHTTPError(err error) error {
	var invalid catalogadmin.InvalidError
	var conflict catalogadmin.ConflictError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &invalid):
		return httpx.ErrCodeDetail(errcode.CommonValidation, invalid.Message)
	case errors.As(err, &conflict):
		return httpx.ErrCodeDetail(errcode.CommonConflict, conflict.Message)
	case errors.Is(err, catalogadmin.ErrNotFound):
		return httpx.ErrCodeDetail(errcode.CommonNotFound, "Upstream configuration not found")
	case errors.Is(err, catalogadmin.ErrDuplicate):
		return httpx.ErrCodeDetail(errcode.CommonConflict,
			"This model already maps to that upstream model name on this provider")
	default:
		return err
	}
}

func (s *Server) ListGatewayRoutes(ctx context.Context, req ListGatewayRoutesRequestObject) (ListGatewayRoutesResponseObject, error) {
	routes, err := s.catalogAdmin.RoutesForModel(ctx, req.ModelId)
	if err != nil {
		return nil, routeHTTPError(err)
	}
	return ListGatewayRoutes200JSONResponse{Items: routesDTO(routes)}, nil
}

func (s *Server) ListGatewayProviderRoutes(ctx context.Context, req ListGatewayProviderRoutesRequestObject) (ListGatewayProviderRoutesResponseObject, error) {
	routes, err := s.catalogAdmin.RoutesForProvider(ctx, req.ProviderId)
	if err != nil {
		return nil, routeHTTPError(err)
	}
	return ListGatewayProviderRoutes200JSONResponse{Items: routesDTO(routes)}, nil
}

func (s *Server) CreateGatewayRoute(ctx context.Context, req CreateGatewayRouteRequestObject) (CreateGatewayRouteResponseObject, error) {
	in := req.Body
	if in == nil || in.ProviderId == nil || in.ProviderModelId == nil {
		return nil, httpx.ErrCodeDetail(errcode.CommonValidation,
			"provider_id and provider_model_id are required")
	}
	envelope, err := routeEnvelopeFromInput(in.VideoEnvelope)
	if err != nil {
		return nil, err
	}
	route, err := s.catalogAdmin.CreateRoute(ctx, catalogadmin.RouteCreate{
		ModelID: req.ModelId, ProviderID: *in.ProviderId,
		ProviderModelID: *in.ProviderModelId,
		Priority:        int32PtrFromInt(in.Priority), Weight: int32PtrFromInt(in.Weight),
		Enabled: in.Enabled, Headers: in.Headers, Quirks: in.Quirks,
		ContextWindow:   int32PtrFromInt(in.ContextWindow),
		MaxOutputTokens: int32PtrFromInt(in.MaxOutputTokens),
		MaxImages:       int32PtrFromInt(in.MaxImages),
		VideoEnvelope:   envelope,
	})
	if err != nil {
		return nil, routeHTTPError(err)
	}
	return CreateGatewayRoute201JSONResponse(routeDTO(route)), nil
}

func (s *Server) UpdateGatewayRoute(ctx context.Context, req UpdateGatewayRouteRequestObject) (UpdateGatewayRouteResponseObject, error) {
	in := req.Body
	if in == nil {
		return nil, httpx.ErrCodeDetail(errcode.CommonValidation, "A request body is required")
	}
	envelope, err := routeEnvelopeFromInput(in.VideoEnvelope)
	if err != nil {
		return nil, err
	}
	patch := catalogadmin.RoutePatch{
		ProviderModelID: in.ProviderModelId,
		Priority:        int32PtrFromInt(in.Priority), Weight: int32PtrFromInt(in.Weight),
		Enabled: in.Enabled, Headers: in.Headers, Quirks: in.Quirks,
		ContextWindow:   int32PtrFromInt(in.ContextWindow),
		MaxOutputTokens: int32PtrFromInt(in.MaxOutputTokens),
		MaxImages:       int32PtrFromInt(in.MaxImages),
		VideoEnvelope:   envelope,
	}
	route, err := s.catalogAdmin.UpdateRoute(ctx, req.ModelId, req.RouteId, patch)
	if err != nil {
		return nil, routeHTTPError(err)
	}
	return UpdateGatewayRoute200JSONResponse(routeDTO(route)), nil
}

func (s *Server) DeleteGatewayRoute(ctx context.Context, req DeleteGatewayRouteRequestObject) (DeleteGatewayRouteResponseObject, error) {
	if err := s.catalogAdmin.DeleteRoute(ctx, req.ModelId, req.RouteId); err != nil {
		return nil, routeHTTPError(err)
	}
	return DeleteGatewayRoute204Response{}, nil
}

// derefModalities unwraps the optional list. Absent and empty are the same
// thing to both write paths -- "say nothing about this" -- so they collapse
// here rather than being told apart downstream.
func derefModalities(v *OutputModalities) []string {
	if v == nil {
		return nil
	}
	return *v
}
