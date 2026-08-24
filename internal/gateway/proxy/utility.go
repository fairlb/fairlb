package proxy

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
)

// RunUtility executes a metering utility operation. Utility requests pass the
// same authentication, model admission, RPM/TPM, route-capability, provider
// capacity and audit controls as inference, but create no wallet hold and no
// consumption charge.
func (p *Pipeline) RunUtility(ctx context.Context, in Request) (Result, *Error) {
	in.Stream = false
	in.Utility = true
	started := time.Now()
	requestID := in.requestID()

	prep, gerr := p.admission.Prepare(ctx, in)
	if gerr != nil {
		p.settlementRecorder.RecordRejection(ctx, prep, in, requestID, gerr, started)
		return Result{}, gerr
	}
	prep, in, gerr = p.pinAffinity(ctx, prep, in)
	if gerr != nil {
		p.settlementRecorder.RecordRejection(ctx, prep, in, requestID, gerr, started)
		return Result{}, gerr
	}

	byok := prep.byok
	upstream, rotation := p.executor.Execute(
		ctx, prep.res.Routes, in, prep.res.Model, byok, prep.id.OrgID, prep.inputTokens,
	)
	if rotation.err != nil {
		p.logFailure(ctx, prep.id, in, prep.modelSlug, rotation.route, rotation, requestID, rotation.err, started)
		return Result{}, rotation.err
	}

	usage := parseUtilityUsage(in.Surface, upstream.body)
	args := settleArgs{
		id: prep.id, requestID: requestID, usage: usage,
		model: prep.res.Model, route: rotation.route, in: in,
		pricing: prep.pricing, priceTable: prep.priceTable,
		httpStatus:       upstream.status,
		durationMs:       int32(time.Since(started).Milliseconds()),
		attempts:         int32(upstream.attempts),
		byok:             upstream.byok,
		orgProviderKeyID: byokKeyOf(upstream),
		providerKeyID:    sharedKeyOf(upstream),
		routeID:          rotation.routeID(),
		trail:            rotation.trailJSON(),
		utility:          true,
	}
	if err := p.gw.InsertUsageLog(ctx, usageLogParams(args)); err != nil {
		slog.ErrorContext(ctx, "dataplane: recording utility operation failed",
			"surface", in.Surface, "request_id", requestID, "error", err)
		return Result{}, NewError(errcode.GatewayInternal, "Audit recording failed")
	}
	recordOutcome(ctx, string(in.Surface), "ok", false, time.Duration(args.durationMs)*time.Millisecond)
	return Result{Status: upstream.status, Body: upstream.body}, nil
}

func parseUtilityUsage(surface catalog.Surface, body []byte) Usage {
	switch surface {
	case catalog.SurfaceMessagesCountTokens:
		var r struct {
			InputTokens int64 `json:"input_tokens"`
		}
		if json.Unmarshal(body, &r) == nil {
			return Usage{In: r.InputTokens, Present: true}
		}
	case catalog.SurfaceGeminiCountTokens:
		var r struct {
			TotalTokens             int64 `json:"totalTokens"`
			CachedContentTokenCount int64 `json:"cachedContentTokenCount"`
		}
		if json.Unmarshal(body, &r) == nil {
			return Usage{
				In:         subsetIn(r.TotalTokens, r.CachedContentTokenCount, 0),
				CachedRead: r.CachedContentTokenCount, Present: true,
			}
		}
	case catalog.SurfaceResponsesInputTokens:
		var r struct {
			InputTokens int64 `json:"input_tokens"`
		}
		if json.Unmarshal(body, &r) == nil {
			return Usage{In: r.InputTokens, Present: true}
		}
	}
	return Usage{}
}
