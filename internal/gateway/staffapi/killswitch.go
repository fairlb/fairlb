package gwstaffapi

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/fairlb/fairlb/audit"
	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/foundation/publicid"
)

// The global kill switch, plus the facts the UI needs about this deployment.
//
// The gateway.kill_switch setting is owned by the gateway layer, so it gets a
// dedicated endpoint here rather than being reached through a generic
// settings-by-key surface. An emergency stop is not something to route through
// a generic path that may or may not be mounted: the failure mode of getting
// that wrong is a page that renders correctly, with a button that is clickable,
// whose request 404s.

// GetGatewayMeta returns the verdict, not the inputs behind it.
//
// Whether the connectivity probe may return its full trace is one rule. Written
// once on the server and once again in the client, the two copies drift, and
// the drift shows up as a control that is visible and does nothing. So the
// server states the conclusion and the client asks for it.
func (s *Server) GetGatewayMeta(ctx context.Context, _ GetGatewayMetaRequestObject) (GetGatewayMetaResponseObject, error) {
	if _, err := httpx.RequireUserID(ctx); err != nil {
		return nil, err
	}
	return GetGatewayMeta200JSONResponse{ProbeTraceAllowed: s.traceEnabled}, nil
}

func (s *Server) GetGatewayKillSwitch(ctx context.Context, _ GetGatewayKillSwitchRequestObject) (GetGatewayKillSwitchResponseObject, error) {
	if _, err := httpx.RequireUserID(ctx); err != nil {
		return nil, err
	}
	active, at, by, err := s.catalog.Settings().KillSwitchState(ctx)
	if err != nil {
		return nil, fmt.Errorf("gwstaffapi: read kill-switch state: %w", err)
	}
	resp := GetGatewayKillSwitch200JSONResponse{Active: active}
	if !at.IsZero() {
		resp.UpdatedAt = &at
	}
	if by != "" {
		resp.UpdatedBy = &by
	}
	return resp, nil
}

// PutGatewayKillSwitch pulls or restores the switch. It takes the highest
// privilege, because one call stops all traffic.
func (s *Server) PutGatewayKillSwitch(ctx context.Context, req PutGatewayKillSwitchRequestObject) (PutGatewayKillSwitchResponseObject, error) {
	staffID, err := httpx.RequireSuperadmin(ctx)
	if err != nil {
		return nil, err
	}
	if req.Body == nil || strings.TrimSpace(req.Body.Reason) == "" {
		return nil, httpx.ErrCodeDetail(errcode.CommonValidation, "A reason is required")
	}
	if err := s.catalog.Settings().SetKillSwitch(ctx, req.Body.Active, publicid.UUIDString(staffID)); err != nil {
		return nil, err
	}

	// A detailed audit entry. The middleware's fallback records only who
	// called this endpoint and when, while the reason is in the body -- and
	// the reason for pulling the switch and the reason for restoring it are
	// entirely different incident records. Without it the audit trail is just
	// a timestamp. Marking the request audited afterwards suppresses the
	// fallback row so there is exactly one.
	action := "gateway.kill_switch.restore"
	if req.Body.Active {
		action = "gateway.kill_switch.pull"
	}
	meta, _ := json.Marshal(map[string]any{"reason": req.Body.Reason, "active": req.Body.Active})
	if err := audit.InsertTx(ctx, s.pool, audit.Entry{
		ActorType:  "staff",
		ActorID:    staffID,
		Action:     action,
		TargetType: "gateway_setting",
		TargetID:   "gateway.kill_switch",
		Meta:       meta,
		RequestID:  httpx.RequestIDFrom(ctx),
	}); err != nil {
		return nil, fmt.Errorf("gwstaffapi: write kill-switch audit record: %w", err)
	}
	httpx.MarkAudited(ctx)
	return PutGatewayKillSwitch204Response{}, nil
}
