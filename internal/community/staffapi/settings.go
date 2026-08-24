package staffapi

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/settings"
	settingsadmin "github.com/fairlb/fairlb/settings/admin"
)

// The settings surface (ADR-0198). It lists and writes exactly the registry this
// deployment assembled — the gateway's layer — and the write goes through the
// same Writer the hosted product's staff plane uses: the reason policy, the
// section rules and the audit rows are one implementation, not one per product.

func (s *Server) CommunityListSettings(ctx context.Context, _ CommunityListSettingsRequestObject) (CommunityListSettingsResponseObject, error) {
	if _, err := httpx.RequireUserID(ctx); err != nil {
		return nil, err
	}
	if s.settings == nil {
		return nil, httpx.ErrCodeDetail(errcode.CommonInternal, "No settings registry is assembled")
	}
	entries, err := s.settings.List(ctx)
	if err != nil {
		return nil, err
	}
	resp := CommunityListSettings200JSONResponse{Items: make([]SettingEntry, 0, len(entries))}
	for _, e := range entries {
		kind, ok := settingKind(e.Spec.Kind)
		if !ok {
			// Fail loud rather than cast: a Kind the contract does not list
			// would otherwise reach a client whose generated type lacks it.
			return nil, httpx.ErrCodeDetail(errcode.CommonInternal,
				"Setting "+e.Spec.Key+" has kind "+string(e.Spec.Kind)+", which the API contract does not list")
		}
		item := SettingEntry{
			Key: e.Spec.Key, Kind: kind,
			Group: SettingEntryGroup(e.Spec.Group), Impact: SettingEntryImpact(e.Spec.Impact), Set: e.Set,
		}
		if e.Spec.Section != "" {
			section := e.Spec.Section
			item.Section = &section
		}
		if e.Hint != "" {
			hint := e.Hint
			item.Hint = &hint
		}
		if e.Spec.Description != "" {
			item.Description = &e.Spec.Description
		}
		if e.Spec.DescriptionKey != "" {
			item.DescriptionKey = &e.Spec.DescriptionKey
		}
		if len(e.Spec.Enum) > 0 {
			enum := e.Spec.Enum
			item.Enum = &enum
		}
		if r := e.Spec.Range; r != nil {
			min, max := r.Min, r.Max
			item.Min, item.Max = &min, &max
		}
		if e.Value != nil {
			var v any
			if err := json.Unmarshal(e.Value, &v); err == nil {
				item.Value = v
			}
		}
		if len(e.Spec.Default) > 0 {
			var d any
			if err := json.Unmarshal(e.Spec.Default, &d); err == nil {
				item.Default = &d
			}
		}
		if e.Set {
			at, by := e.UpdatedAt, e.UpdatedBy
			item.UpdatedAt, item.UpdatedBy = &at, &by
		}
		resp.Items = append(resp.Items, item)
	}
	return resp, nil
}

// settingKind maps the registry's Kind onto the contract's enum explicitly: a
// string conversion never fails, so a Kind missing from the contract would
// pass silently and surface as a value the client's type does not have.
func settingKind(k settings.Kind) (SettingEntryKind, bool) {
	switch k {
	case settings.KindString:
		return String, true
	case settings.KindEnum:
		return Enum, true
	case settings.KindStringList:
		return StringList, true
	case settings.KindEmail:
		return Email, true
	case settings.KindBool:
		return Bool, true
	case settings.KindInt:
		return Int, true
	case settings.KindDecimalString:
		return DecimalString, true
	case settings.KindMapStringInt:
		return MapStringInt, true
	case settings.KindMoney:
		return Money, true
	case settings.KindSecret:
		return Secret, true
	default:
		return "", false
	}
}

func (s *Server) CommunityPutSettings(ctx context.Context, req CommunityPutSettingsRequestObject) (CommunityPutSettingsResponseObject, error) {
	// 写一律 superadmin，与 Cloud 的运营面同一条线（kill switch、定价已是这一档）
	userID, err := httpx.RequireSuperadmin(ctx)
	if err != nil {
		return nil, err
	}
	if s.settings == nil {
		return nil, httpx.ErrCodeDetail(errcode.CommonInternal, "No settings registry is assembled")
	}
	if req.Body == nil || len(req.Body.Changes) == 0 {
		return nil, httpx.ErrCode(errcode.CommonValidation)
	}
	changes := make([]settings.Change, 0, len(req.Body.Changes))
	for _, c := range req.Body.Changes {
		raw, err := json.Marshal(c.Value)
		if err != nil {
			return nil, httpx.ErrCodeDetail(errcode.CommonValidation, "The value of "+c.Key+" is not valid JSON")
		}
		changes = append(changes, settings.Change{Key: c.Key, Value: raw})
	}
	reason := ""
	if req.Body.Reason != nil {
		reason = strings.TrimSpace(*req.Body.Reason)
	}
	// Community administrators are staff users; the audit table knows that actor type.
	actor := settingsadmin.Actor{Type: "staff", ID: userID, IP: httpx.ClientIPFrom(ctx), RequestID: httpx.RequestIDFrom(ctx)}
	if err := settingsadmin.New(s.pool, s.settings).Put(ctx, actor, changes, reason); err != nil {
		return nil, err
	}
	return CommunityPutSettings204Response{}, nil
}
