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
	return DiscoverProviderModels200JSONResponse(discoverResult(res)), nil
}

// GetProviderDiscoveredModels answers from the stored catalogue, without
// calling upstream.
//
// Same body as the POST, deliberately: the reader's question is the same one,
// and the only difference is who paid for the answer. A provider nobody has
// asked yet comes back with a zero checked_at and no models, which the client
// already has to distinguish from a fetch that succeeded and found nothing.
func (s *Server) GetProviderDiscoveredModels(
	ctx context.Context, req GetProviderDiscoveredModelsRequestObject,
) (GetProviderDiscoveredModelsResponseObject, error) {
	res, err := s.discovery.Snapshot(ctx, req.ProviderId)
	if err != nil {
		if err == discovery.ErrProviderNotFound {
			return nil, httpx.ErrCodeDetail(errcode.CommonNotFound, "Provider not found")
		}
		return nil, err
	}
	return GetProviderDiscoveredModels200JSONResponse(discoverResult(res)), nil
}

func discoverResult(res discovery.Result) DiscoverModelsResult {
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
		// Absent rather than blank when nothing could name the model: an empty
		// suggestion and no suggestion read the same to a person, but only one
		// of them says so to a client.
		if sg := model.Suggestion; sg != nil {
			item.Suggestion = &ModelSuggestion{
				Slug:            sg.Slug,
				DisplayName:     strutil.Ptr(sg.DisplayName),
				ContextWindow:   intPtr(int(sg.ContextWindow)),
				MaxOutputTokens: intPtr(int(sg.MaxOutputTokens)),
				ManualProbe:     boolPtr(sg.ManualProbe),
				// Only the seed knows this, and only for the models it names.
				// Absent everywhere else, which is what lets the create dialog
				// leave the field at its default rather than pre-filling a
				// guess (ADR-0226).
				OutputModalities: modalitiesPtr(sg.OutputModalities),
				Source:           ModelSuggestionSource(sg.Source),
			}
		}
		out.Models = append(out.Models, item)
	}
	return out
}

func boolPtr(v bool) *bool { return &v }

// modalitiesPtr wraps a suggestion's modalities, or nil when the source did not
// know them. Absent and empty read the same to a person; only one of them says
// so to a client.
func modalitiesPtr(v []string) *OutputModalities {
	if len(v) == 0 {
		return nil
	}
	out := OutputModalities(v)
	return &out
}
