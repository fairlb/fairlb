package proxy

import (
	"context"
	"net/http"
	"slices"

	"github.com/fairlb/fairlb/internal/gateway/catalog"
	"github.com/fairlb/fairlb/internal/gateway/video"
)

// GET /v1/videos/models: what each model accepts, and what it costs.
//
// This is not decoration. It is the half that makes normalising parameters safe
// to use: without it a caller can only find the admissible set by trial, and on
// a plane where every attempt is a real charge, trial is not a way to learn
// anything. The refusals on the submit path name what would have been accepted
// for the same reason.
//
// What it publishes is the union across the routes that can actually serve the
// model -- the same union admission validates against -- so "listed here" and
// "accepted there" cannot come apart.

type videoModelCapabilities struct {
	ID                     string   `json:"id"`
	Object                 string   `json:"object"`
	DurationsSeconds       []int    `json:"durations_seconds,omitempty"`
	Resolutions            []string `json:"resolutions,omitempty"`
	AspectRatios           []string `json:"aspect_ratios,omitempty"`
	Audio                  string   `json:"audio,omitempty"`
	MaxN                   int      `json:"max_n"`
	SupportsImage          bool     `json:"supports_image_to_video"`
	SupportsLastFrame      bool     `json:"supports_last_frame"`
	MaxReferenceImages     int      `json:"max_reference_images"`
	SupportsNegativePrompt bool     `json:"supports_negative_prompt"`
	// Cancel is one of never, queued_only or anytime. Published because it
	// genuinely differs between models, and a client that assumes it can stop a
	// job needs to know before it builds a stop button (ADR-0221).
	Cancel string `json:"cancel"`
	// Pricing is the rate card in the caller's own currency, so the cost of a
	// clip is knowable before it is asked for -- which is true on this plane in
	// a way it is not on the token plane.
	Pricing []videoRate `json:"pricing,omitempty"`
}

type videoRate struct {
	Unit        string `json:"unit"`
	Resolution  string `json:"resolution,omitempty"`
	Audio       string `json:"audio,omitempty"`
	NanoPerUnit int64  `json:"nano_per_unit"`
}

// handleVideoModels lists every video model this caller may use.
func (p *Pipeline) handleVideoModels() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, gerr := p.auth.Authenticate(r.Context(), CredentialOf(r))
		if gerr != nil {
			Write(w, SurfaceOpenAI, gerr)
			return
		}
		if gerr := RequireScope(id, "inference"); gerr != nil {
			Write(w, SurfaceOpenAI, gerr)
			return
		}
		models, err := p.catalog.ModelsForOrg(r.Context(), id.ModelTierID, id.OrgID)
		if err != nil {
			Write(w, SurfaceOpenAI, NewError("gateway.internal", "The catalog could not be read"))
			return
		}
		out := make([]videoModelCapabilities, 0)
		for _, m := range models {
			if !slices.Contains(m.Endpoints, "video") {
				continue
			}
			// Admission narrows by tier and key allowlist; so does this. A
			// model listed here that the caller cannot call is the failure the
			// catalog exists to prevent.
			if gerr := p.guard.CheckModel(id, m.Slug); gerr != nil {
				continue
			}
			caps, ok := p.videoCapabilities(r.Context(), id, m.Slug)
			if !ok {
				continue
			}
			out = append(out, caps)
		}
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": out})
	}
}

// videoCapabilities resolves one model's published envelope and rate card.
func (p *Pipeline) videoCapabilities(
	ctx context.Context, id Identity, slug string,
) (videoModelCapabilities, bool) {
	res, err := p.catalog.ResolveFor(ctx, slug, catalog.SurfaceVideo, id.ModelTierID, nil)
	if err != nil {
		return videoModelCapabilities{}, false
	}
	accepts := video.Union(routeEnvelopes(ctx, res.Routes))
	if !accepts.Configured() {
		// No route says what it accepts, so there is nothing truthful to
		// publish. Listing it with an empty envelope would read as "accepts
		// anything", which is the opposite of what it means.
		return videoModelCapabilities{}, false
	}
	caps := videoModelCapabilities{
		ID: slug, Object: "video_model",
		DurationsSeconds: accepts.DurationsSeconds, Resolutions: accepts.Resolutions,
		AspectRatios: accepts.AspectRatios, Audio: string(accepts.Audio),
		MaxN: max(accepts.MaxN, 1), SupportsImage: accepts.SupportsImageToVideo,
		SupportsLastFrame:      accepts.SupportsLastFrame,
		MaxReferenceImages:     accepts.MaxReferenceImages,
		SupportsNegativePrompt: accepts.SupportsNegativePrompt,
		Cancel:                 string(accepts.CancelModeOrDefault()),
	}
	table, err := p.catalog.LockedUnitPriceTable(ctx, res.Model.ID)
	if err != nil {
		return caps, true
	}
	for _, rate := range table.Snapshot() {
		caps.Pricing = append(caps.Pricing, videoRate{
			Unit: rate.Unit, Resolution: rate.Resolution,
			Audio: rate.Audio, NanoPerUnit: rate.NanoPerUnit,
		})
	}
	return caps, true
}
