package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
)

const (
	resourceResponse    = "response"
	resourceInteraction = "interaction"
)

// affinityResolutionError preserves resource semantics when catalog
// resolution has no candidates. Resolution normally runs before pinning; if
// the only original route is disabled, returning model_not_found here would
// turn a known state id into a misleading 404 instead of the required 503.
func (p *Pipeline) affinityResolutionError(
	ctx context.Context, id Identity, in Request, cached *gwdb.GetResourceAffinityRow,
) *Error {
	resourceType, upstreamID := affinityReference(in)
	if upstreamID == "" {
		return nil
	}
	if cached != nil {
		return NewError(errcode.GatewayStateRouteUnavailable, "The resource's original route is unavailable")
	}
	_, err := p.gw.GetResourceAffinity(ctx, gwdb.GetResourceAffinityParams{
		OrgID: id.OrgID, Protocol: string(in.Protocol),
		ResourceType: resourceType, UpstreamID: upstreamID,
	})
	switch {
	case err == nil:
		return NewError(errcode.GatewayStateRouteUnavailable, "The resource's original route is unavailable")
	case errors.Is(err, pgx.ErrNoRows):
		return NewError(errcode.GatewayResourceNotFound, "Resource not found or expired")
	default:
		slog.ErrorContext(ctx, "dataplane: reading resource affinity during model resolution failed", "error", err)
		return NewError(errcode.GatewayInternal, "Resource lookup failed")
	}
}

// modelForAdmission keeps stateful requests native: Responses compact/count
// and Gemini Interactions may identify a stored conversation without repeating
// its model. The org-scoped affinity is the only safe source for that model.
func (p *Pipeline) modelForAdmission(
	ctx context.Context, id Identity, in Request,
) (string, *gwdb.GetResourceAffinityRow, *Error) {
	model, modelErr := modelForRequest(in)
	if modelErr == nil {
		return model, nil, nil
	}
	resourceType, upstreamID := affinityReference(in)
	if upstreamID == "" {
		return "", nil, NewError(errcode.GatewayInvalidRequest, modelErr.Error())
	}
	affinity, err := p.gw.GetResourceAffinity(ctx, gwdb.GetResourceAffinityParams{
		OrgID: id.OrgID, Protocol: string(in.Protocol),
		ResourceType: resourceType, UpstreamID: upstreamID,
	})
	switch {
	case err == nil:
		return affinity.ModelSlug, &affinity, nil
	case errors.Is(err, pgx.ErrNoRows):
		return "", nil, NewError(errcode.GatewayResourceNotFound, "Resource not found or expired")
	default:
		slog.ErrorContext(ctx, "dataplane: reading resource affinity for model failed", "error", err)
		return "", nil, NewError(errcode.GatewayInternal, "Resource lookup failed")
	}
}

// pinAffinity turns a persisted upstream id into a one-route, one-credential
// request. An org-scoped miss is always a native 404; an affinity whose route
// or credential no longer works is a 503 and is never silently sent elsewhere.
func (p *Pipeline) pinAffinity(
	ctx context.Context, prep prepared, in Request,
) (prepared, Request, *Error) {
	resourceType, upstreamID := affinityReference(in)
	if upstreamID == "" {
		return prep, in, nil
	}
	var affinity gwdb.GetResourceAffinityRow
	if prep.affinity != nil {
		affinity = *prep.affinity
	} else {
		var err error
		affinity, err = p.gw.GetResourceAffinity(ctx, gwdb.GetResourceAffinityParams{
			OrgID: prep.id.OrgID, Protocol: string(in.Protocol),
			ResourceType: resourceType, UpstreamID: upstreamID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return prep, in, NewError(errcode.GatewayResourceNotFound, "Resource not found or expired")
		}
		if err != nil {
			slog.ErrorContext(ctx, "dataplane: reading resource affinity failed", "error", err)
			return prep, in, NewError(errcode.GatewayInternal, "Resource lookup failed")
		}
	}
	if affinity.ModelID != prep.res.Model.ID || !affinity.RouteID.Valid || !affinity.ProviderID.Valid {
		return prep, in, NewError(errcode.GatewayStateRouteUnavailable, "The resource's original route is unavailable")
	}
	var pinned []catalog.Route
	for _, route := range prep.res.Routes {
		if route.ID == affinity.RouteID && route.ProviderID == affinity.ProviderID {
			pinned = append(pinned, route)
			break
		}
	}
	if len(pinned) != 1 || !p.breaker.ProviderAvailable(ctx, pinned[0].ProviderID) {
		return prep, in, NewError(errcode.GatewayStateRouteUnavailable, "The resource's original route is unavailable")
	}
	if !affinity.ProviderKeyID.Valid && !affinity.OrgProviderKeyID.Valid {
		return prep, in, NewError(errcode.GatewayStateRouteUnavailable, "The resource's original credential is unavailable")
	}
	prep.res.Routes = pinned
	in.PinnedProviderKeyID = affinity.ProviderKeyID
	in.PinnedOrgProviderKeyID = affinity.OrgProviderKeyID
	return prep, in, nil
}

func affinityReference(in Request) (resourceType, upstreamID string) {
	if in.Resource != "" {
		switch in.Surface {
		case catalog.SurfaceResponsesResources:
			return resourceResponse, in.Resource
		case catalog.SurfaceGeminiInteractions:
			return resourceInteraction, in.Resource
		}
	}
	var probe struct {
		PreviousResponseID    string `json:"previous_response_id"`
		PreviousInteractionID string `json:"previous_interaction_id"`
	}
	if json.Unmarshal(in.Body, &probe) != nil {
		return "", ""
	}
	switch in.Surface {
	case catalog.SurfaceResponses, catalog.SurfaceResponsesCompact, catalog.SurfaceResponsesInputTokens:
		return resourceResponse, probe.PreviousResponseID
	case catalog.SurfaceGeminiInteractions:
		return resourceInteraction, probe.PreviousInteractionID
	default:
		return "", ""
	}
}

func (p *Pipeline) rememberAffinity(
	ctx context.Context, prep prepared, in Request, upstream upstreamResult, rotation rotationResult,
) {
	switch in.Surface {
	case catalog.SurfaceResponses:
	case catalog.SurfaceGeminiInteractions:
	default:
		return
	}
	if !requestStoresResource(in.Body) {
		return
	}
	var response struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(upstream.body, &response) != nil || response.ID == "" {
		return
	}
	p.rememberAffinityID(ctx, prep, in, response.ID, upstream, rotation)
}

func (p *Pipeline) rememberAffinityID(
	ctx context.Context, prep prepared, in Request, upstreamID string,
	upstream upstreamResult, rotation rotationResult,
) {
	resourceType := ""
	switch in.Surface {
	case catalog.SurfaceResponses:
		resourceType = resourceResponse
	case catalog.SurfaceGeminiInteractions:
		resourceType = resourceInteraction
	default:
		return
	}
	if upstreamID == "" || !requestStoresResource(in.Body) {
		return
	}
	err := p.gw.UpsertResourceAffinity(ctx, gwdb.UpsertResourceAffinityParams{
		OrgID: prep.id.OrgID, Protocol: string(in.Protocol), ResourceType: resourceType,
		UpstreamID: upstreamID, ModelID: prep.res.Model.ID,
		RouteID: rotation.route.ID, ProviderID: rotation.route.ProviderID,
		ProviderKeyID: sharedKeyOf(upstream), OrgProviderKeyID: byokKeyOf(upstream),
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(p.catalog.Settings().ResourceAffinityTTL(ctx)), Valid: true},
	})
	if err != nil {
		// The inference still settles and returns: hiding a successful, charged
		// upstream response would make a retry create a second resource. This is
		// an operator-visible durability fault, not a reason to duplicate work.
		slog.ErrorContext(ctx, "dataplane: persisting resource affinity failed",
			"resource_type", resourceType, "request_id", in.RequestID, "error", err)
	}
}

func requestStoresResource(body []byte) bool {
	var probe struct {
		Store *bool `json:"store"`
	}
	if json.Unmarshal(body, &probe) != nil {
		return false
	}
	return probe.Store == nil || *probe.Store
}

func (p *Pipeline) deleteAffinity(ctx context.Context, orgID pgtype.UUID, protocol Protocol, resourceType, id string) {
	if err := p.gw.DeleteResourceAffinity(ctx, gwdb.DeleteResourceAffinityParams{
		OrgID: orgID, Protocol: string(protocol), ResourceType: resourceType, UpstreamID: id,
	}); err != nil {
		slog.ErrorContext(ctx, "dataplane: deleting resource affinity failed", "resource_type", resourceType, "error", err)
	}
}

func (p *Pipeline) resourceModel(
	ctx context.Context, credential string, protocol Protocol, resourceType, upstreamID string,
) (Identity, string, *Error) {
	id, gerr := p.auth.Authenticate(ctx, credential)
	if gerr != nil {
		return Identity{}, "", gerr
	}
	if gerr := RequireScope(id, "inference"); gerr != nil {
		return Identity{}, "", gerr
	}
	affinity, err := p.gw.GetResourceAffinity(ctx, gwdb.GetResourceAffinityParams{
		OrgID: id.OrgID, Protocol: string(protocol), ResourceType: resourceType, UpstreamID: upstreamID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Identity{}, "", NewError(errcode.GatewayResourceNotFound, "Resource not found or expired")
	}
	if err != nil {
		return Identity{}, "", NewError(errcode.GatewayInternal, "Resource lookup failed")
	}
	return id, affinity.ModelSlug, nil
}
