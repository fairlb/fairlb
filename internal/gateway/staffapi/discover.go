package gwstaffapi

import (
	"context"
	"github.com/fairlb/fairlb/foundation/strutil"

	"github.com/google/uuid"

	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/internal/gateway/discovery"
)

// Upstream model discovery, as a contract.
//
// The whole of it — the fetch, the pagination, the two dialects, the
// classification — lives in internal/gateway/discovery (ADR-0177). What is left
// here is that a failed probe is still a 200: not being able to reach the
// upstream is the *result* of this endpoint, not a fault in it. Expressing it as
// 4xx/5xx would force clients to tell "the probe ran and did not pass" from
// "the probe endpoint itself is broken", which is the same reason the
// connectivity probe answers 200 either way.

func (s *Server) DiscoverProviderModels(
	ctx context.Context, req DiscoverProviderModelsRequestObject,
) (DiscoverProviderModelsResponseObject, error) {
	res, err := s.discovery.Discover(ctx, req.ProviderId)
	if err != nil {
		if err == discovery.ErrProviderNotFound {
			return nil, httpx.ErrCodeDetail(errcode.CommonNotFound, "Provider not found")
		}
		return nil, err
	}
	out := DiscoverModelsResult{
		CheckedAt: res.CheckedAt, Ok: res.Ok, Complete: res.Complete,
		StatusCode: res.StatusCode, Message: strutil.Ptr(res.Message),
		Models: make([]DiscoveredModel, 0, len(res.Models)),
	}
	for _, m := range res.Models {
		model := m
		item := DiscoveredModel{
			UpstreamModel: model.UpstreamModel,
			State:         DiscoveredModelState(model.State),
			ModelSlug:     strutil.Ptr(model.ModelSlug),
		}
		// A zero id is "no local model at all", which the contract spells as an
		// absent field rather than a zero uuid.
		if model.ModelID != (uuid.UUID{}) {
			id := model.ModelID
			item.ModelId = &id
		}
		out.Models = append(out.Models, item)
	}
	return DiscoverProviderModels200JSONResponse(out), nil
}
