package gwstaffapi

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/internal/gateway/video"
)

// Translation of a route's video capability envelope between the contract and
// the column.
//
// Both directions go through video.Envelope rather than copying field by field.
// That type is where the envelope's shape is defined and where the data plane
// reads it from, so routing the admin API through it means a field added there
// and forgotten here fails to compile instead of quietly never being editable.
// It also throws away anything the column happens to hold that the type does
// not name: this envelope decides whether requests are admitted, and a key
// nothing reads has no business surviving a round trip through the editor.

func envelopeFromMap(raw map[string]any) (video.Envelope, bool) {
	if len(raw) == 0 {
		return video.Envelope{}, false
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return video.Envelope{}, false
	}
	e, err := video.ParseEnvelope(encoded)
	if err != nil {
		// An unreadable envelope is reported as absent rather than as an error,
		// matching what admission does with one: it covers nothing, so no
		// request is served by that route. Failing the whole route listing
		// instead would take every other route on the model off the screen.
		return video.Envelope{}, false
	}
	return e, true
}

// videoEnvelopeDTO renders a route's stored envelope, or nil where there is
// none. Most routes are not video routes, and an empty object would read as
// "declared to accept nothing" rather than "nothing declared".
func videoEnvelopeDTO(raw map[string]any) *VideoEnvelope {
	e, ok := envelopeFromMap(raw)
	if !ok {
		return nil
	}
	out := VideoEnvelope{
		MaxN:                   int32Ptr(e.MaxN),
		MaxReferenceImages:     int32Ptr(e.MaxReferenceImages),
		MaxPromptChars:         int32Ptr(e.MaxPromptChars),
		MaxJobSeconds:          int32Ptr(e.MaxJobSeconds),
		SupportsImageToVideo:   trueOrNil(e.SupportsImageToVideo),
		SupportsLastFrame:      trueOrNil(e.SupportsLastFrame),
		SupportsNegativePrompt: trueOrNil(e.SupportsNegativePrompt),
	}
	if len(e.DurationsSeconds) > 0 {
		d := make([]int32, 0, len(e.DurationsSeconds))
		for _, v := range e.DurationsSeconds {
			d = append(d, int32(v))
		}
		out.DurationsSeconds = &d
	}
	if len(e.Resolutions) > 0 {
		v := append([]string(nil), e.Resolutions...)
		out.Resolutions = &v
	}
	if len(e.AspectRatios) > 0 {
		v := append([]string(nil), e.AspectRatios...)
		out.AspectRatios = &v
	}
	if e.Audio != "" {
		a := VideoEnvelopeAudio(e.Audio)
		out.Audio = &a
	}
	// Unset reads as never, and it is rendered that way rather than left
	// absent: an operator who has not said a model can be stopped has not
	// promised that it can, and the interface should show the promise it will
	// actually keep.
	c := VideoEnvelopeCancel(e.CancelModeOrDefault())
	out.Cancel = &c
	// Always what is stored, and the write path only ever stores `declared`.
	// The rule this serves is that the interface must never present somebody's
	// entry as an observation.
	src := VideoEnvelopeSource(e.Source)
	if src == "" {
		src = Declared
	}
	out.Source = &src
	return &out
}

// videoEnvelopeMap turns a submitted envelope into what the column stores.
//
// `source` is ignored and stamped: it is read-only in the contract, and a write
// path that honoured it would let the editor label a typed-in envelope as
// observed. Nothing in this build observes one.
func videoEnvelopeMap(in *VideoEnvelope) (map[string]any, error) {
	e := video.Envelope{
		MaxN:                   int(valueOr(in.MaxN, 0)),
		MaxReferenceImages:     int(valueOr(in.MaxReferenceImages, 0)),
		MaxPromptChars:         int(valueOr(in.MaxPromptChars, 0)),
		MaxJobSeconds:          int(valueOr(in.MaxJobSeconds, 0)),
		SupportsImageToVideo:   valueOr(in.SupportsImageToVideo, false),
		SupportsLastFrame:      valueOr(in.SupportsLastFrame, false),
		SupportsNegativePrompt: valueOr(in.SupportsNegativePrompt, false),
		Audio:                  video.AudioSupport(string(valueOr(in.Audio, ""))),
		Cancel:                 video.CancelMode(string(valueOr(in.Cancel, ""))),
		Source:                 declaredEnvelopeSource,
	}
	if in.DurationsSeconds != nil {
		for _, v := range *in.DurationsSeconds {
			e.DurationsSeconds = append(e.DurationsSeconds, int(v))
		}
	}
	if in.Resolutions != nil {
		e.Resolutions = normalizedList(*in.Resolutions)
	}
	if in.AspectRatios != nil {
		e.AspectRatios = normalizedList(*in.AspectRatios)
	}
	// An envelope that declares nothing is stored as an empty object rather
	// than as a lone provenance stamp: `declared` is a claim about a
	// declaration, and there is not one. It is also what makes clearing
	// reachable -- saving a blank form leaves the route in exactly the state a
	// route that never had an envelope is in.
	if !e.Configured() {
		return map[string]any{}, nil
	}
	encoded, err := json.Marshal(e)
	if err != nil {
		return nil, httpx.ErrCodeDetail(errcode.CommonValidation, "the video envelope could not be stored")
	}
	var out map[string]any
	if err := json.Unmarshal(encoded, &out); err != nil {
		return nil, httpx.ErrCodeDetail(errcode.CommonValidation, "the video envelope could not be stored")
	}
	return out, nil
}

// routeEnvelopeFromInput reads the envelope off a route write.
//
// Two-valued, matching the contract: absent leaves the stored envelope alone,
// and an object replaces it whole. A partial merge was the alternative and it
// is the wrong one here -- the envelope's lists are what admission refuses
// against, and "add a duration without saying which ones remain" has no safe
// reading.
//
// There is deliberately no third value for "clear it". An empty object already
// stores an envelope that declares nothing, which admission reads as a route
// serving no video at all, and the column is NOT NULL so no other state exists
// to reach. A `null` variant would have decoded to the same nil pointer an
// omitted field does, so the contract would have promised a clear that quietly
// left the old envelope in place.
func routeEnvelopeFromInput(in *VideoEnvelope) (*map[string]any, error) {
	if in == nil {
		return nil, nil
	}
	m, err := videoEnvelopeMap(in)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// declaredEnvelopeSource is the only value this API writes. `observed` belongs
// to a capability-discovery path, and no vendor in this build publishes one.
const declaredEnvelopeSource = "declared"

func normalizedList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func int32Ptr(v int) *int32 {
	if v == 0 {
		return nil
	}
	n := int32(v)
	return &n
}

// trueOrNil omits a false flag rather than rendering it. Every boolean in the
// envelope means "this deployment does not do that" when absent, so an explicit
// false and an omission say the same thing -- and the JSON the column holds
// omits them for the same reason.
func trueOrNil(v bool) *bool {
	if !v {
		return nil
	}
	return &v
}

// GetGatewayVendorVideoEnvelope answers with what this build's mapper records a
// vendor's model as accepting, for prefilling a route's envelope.
//
// It reads no database and stores nothing. The value is a prefill, and the
// response says so by coming back marked `declared`: these numbers were read
// out of a vendor's published contract when the mapper was written, not
// observed from the vendor now. From the moment somebody saves them, the person
// who saved them is the one answering for the configuration.
func (s *Server) GetGatewayVendorVideoEnvelope(
	ctx context.Context, req GetGatewayVendorVideoEnvelopeRequestObject,
) (GetGatewayVendorVideoEnvelopeResponseObject, error) {
	if _, err := httpx.RequireUserID(ctx); err != nil {
		return nil, err
	}
	upstream := strings.TrimSpace(req.Params.UpstreamModel)
	if upstream == "" {
		return nil, httpx.ErrCodeDetail(errcode.CommonValidation, "upstream_model is required")
	}
	mapper, ok := video.MapperFor(req.Vendor)
	if !ok {
		return nil, httpx.ErrCodeDetail(errcode.CommonNotFound,
			"this build has no video mapper for vendor "+req.Vendor)
	}
	e := mapper.Envelope(upstream)
	e.Source = declaredEnvelopeSource
	encoded, err := json.Marshal(e)
	if err != nil {
		return nil, httpx.ErrCodeDetail(errcode.CommonInternal, "the vendor envelope could not be read")
	}
	var raw map[string]any
	if err := json.Unmarshal(encoded, &raw); err != nil {
		return nil, httpx.ErrCodeDetail(errcode.CommonInternal, "the vendor envelope could not be read")
	}
	dto := videoEnvelopeDTO(raw)
	if dto == nil {
		return nil, httpx.ErrCodeDetail(errcode.CommonNotFound,
			"this build records no video envelope for "+req.Vendor+"/"+upstream)
	}
	return GetGatewayVendorVideoEnvelope200JSONResponse{Envelope: *dto}, nil
}
