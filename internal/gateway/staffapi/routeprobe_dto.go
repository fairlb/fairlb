package gwstaffapi

import (
	"github.com/fairlb/fairlb/foundation/strutil"
	"github.com/google/uuid"

	"github.com/fairlb/fairlb/internal/gateway/routeprobe"
)

// Translation between the route-probe domain and this contract's DTOs.

func probeResultDTO(r routeprobe.Result) GatewayProviderTestResult {
	out := GatewayProviderTestResult{
		CheckedAt: r.CheckedAt, Ok: r.OK,
		LatencyMs: r.LatencyMs, StatusCode: r.StatusCode, Message: r.Message,
	}
	if r.KeyID != (uuid.UUID{}) {
		id := r.KeyID
		out.KeyId = &id
	}
	if r.Trace != nil {
		out.Trace = &GatewayProbeTrace{
			Url: r.Trace.URL, Request: r.Trace.Request,
			ResponseStatus: r.Trace.ResponseStatus, Response: r.Trace.Response,
			Truncated: r.Trace.Truncated,
		}
	}
	return out
}

func probeVerdictsList(verdicts []routeprobe.Verdict) []GatewayRouteProbe {
	out := make([]GatewayRouteProbe, 0, len(verdicts))
	for _, v := range verdicts {
		item := GatewayRouteProbe{
			Endpoint:   GatewayRouteProbeEndpoint(v.Endpoint),
			ProbeMode:  GatewayRouteProbeProbeMode(v.ProbeMode),
			Status:     GatewayRouteProbeStatus(v.Status),
			Source:     GatewayRouteProbeSource(v.Source),
			LatencyMs:  v.LatencyMs,
			StatusCode: v.StatusCode,
			Error:      strutil.Ptr(v.Error),
		}
		if !v.CheckedAt.IsZero() {
			at := v.CheckedAt
			item.CheckedAt = &at
		}
		if !v.EnqueuedAt.IsZero() {
			at := v.EnqueuedAt
			item.ProbeEnqueuedAt = &at
		}
		out = append(out, item)
	}
	return out
}

// int32PtrFromInt narrows the contract's *int to the domain's *int32.
func int32PtrFromInt(v *int) *int32 {
	if v == nil {
		return nil
	}
	n := int32(*v)
	return &n
}
