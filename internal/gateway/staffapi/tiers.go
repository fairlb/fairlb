package gwstaffapi

import (
	"context"
	"errors"
	"fmt"
	"github.com/fairlb/fairlb/foundation/strutil"
	"strings"

	"github.com/fairlb/fairlb/foundation/cursorpage"
	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/internal/gateway/tiers"
)

// Admission tiers: which models an organization may reach.
//
// The rules live in internal/gateway/tiers (ADR-0176). What is left here is the
// contract: shape validation, turning the domain's refusals into status codes,
// and the DTO mapping.

// tierHTTPError maps the domain's refusals.
//
// Each one keeps its own status and its own sentence, because they call for
// different actions: a taken slug means pick another name, a protected default
// means promote a different tier first, and a tier with members names how many
// have to be moved.
func tierHTTPError(err error) error {
	var members tiers.MembersError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, tiers.ErrNotFound):
		return httpx.ErrCodeDetail(errcode.CommonNotFound, "Access tier not found")
	case errors.Is(err, tiers.ErrSlugTaken):
		return httpx.ErrCodeDetail(errcode.CommonConflict, "That slug is already taken")
	case errors.Is(err, tiers.ErrDefaultProtected):
		return httpx.ErrCodeDetail(errcode.CommonConflict,
			"The default tier cannot be disabled or deleted; make another tier the default first")
	case errors.Is(err, tiers.ErrNotSettableAsDefault):
		return httpx.ErrCodeDetail(errcode.CommonConflict,
			"No such tier, or it is disabled; a disabled tier cannot be made the default")
	case errors.Is(err, tiers.ErrAllowAllConflict):
		return httpx.ErrCodeDetail(errcode.CommonConflict,
			"This tier admits every model, so it cannot also list models. "+
				"Turn off allow_all_models first.")
	case errors.Is(err, tiers.ErrUnknownModel):
		return httpx.ErrCodeDetail(errcode.CommonValidation,
			"model_ids contains a model that does not exist")
	case errors.As(err, &members):
		return httpx.ErrCodeDetail(errcode.CommonConflict,
			fmt.Sprintf("%d organisations are still on this tier. Move them to "+
				"another tier first; deleting it would silently change what "+
				"they are allowed to reach.", members.Count))
	default:
		return err
	}
}

func tierDTO(t tiers.Tier) GatewayTier {
	out := GatewayTier{
		Id: t.ID, Slug: t.Slug, Name: strutil.Ptr(t.Name), Description: strutil.Ptr(t.Description),
		AllowAllModels: t.AllowAllModels, IsDefault: t.IsDefault,
		Status: GatewayTierStatus(t.Status),
	}
	if !t.CreatedAt.IsZero() {
		at := t.CreatedAt
		out.CreatedAt = &at
	}
	if !t.UpdatedAt.IsZero() {
		at := t.UpdatedAt
		out.UpdatedAt = &at
	}
	return out
}

func tierModelsDTO(models []tiers.Model) []GatewayTierModel {
	out := make([]GatewayTierModel, 0, len(models))
	for _, m := range models {
		model := m
		out = append(out, GatewayTierModel{
			Id: model.ID, Slug: model.Slug, DisplayName: strutil.Ptr(model.DisplayName),
			Enabled:    &model.Enabled,
			Visibility: (*GatewayTierModelVisibility)(&model.Visibility),
		})
	}
	return out
}

func (s *Server) ListGatewayTiers(ctx context.Context, req ListGatewayTiersRequestObject) (ListGatewayTiersResponseObject, error) {
	page, err := httpx.ParseKeyPage(
		req.Params.Cursor, req.Params.Limit, tiers.TierCursorParts, 50, 200)
	if err != nil {
		return nil, err
	}
	list, err := s.tiers.List(ctx, page, derefOr(req.Params.Q, ""))
	if err != nil {
		return nil, tierHTTPError(err)
	}
	kept, more := cursorpage.Trim(list, int(page.Limit))
	data := make([]GatewayTier, 0, len(kept))
	for _, t := range kept {
		item := tierDTO(t)
		item.ModelCount, item.OrgCount = t.ModelCount, t.OrgCount
		data = append(data, item)
	}
	resp := ListGatewayTiers200JSONResponse{Items: data}
	if more {
		nc := tiers.TierCursor(kept[len(kept)-1])
		resp.NextCursor = &nc
	}
	return resp, nil
}

func (s *Server) CreateGatewayTier(ctx context.Context, req CreateGatewayTierRequestObject) (CreateGatewayTierResponseObject, error) {
	in := req.Body
	if in == nil || in.Slug == nil || strings.TrimSpace(*in.Slug) == "" {
		return nil, httpx.ErrCodeDetail(errcode.CommonValidation, "slug is required")
	}
	tier, err := s.tiers.Create(ctx, tiers.Create{
		Slug:           strings.TrimSpace(*in.Slug),
		Name:           derefOr(in.Name, ""),
		Description:    derefOr(in.Description, ""),
		AllowAllModels: in.AllowAllModels != nil && *in.AllowAllModels,
	})
	if err != nil {
		return nil, tierHTTPError(err)
	}
	return CreateGatewayTier201JSONResponse(tierDTO(tier)), nil
}

func (s *Server) UpdateGatewayTier(ctx context.Context, req UpdateGatewayTierRequestObject) (UpdateGatewayTierResponseObject, error) {
	in := req.Body
	if in == nil {
		return nil, httpx.ErrCodeDetail(errcode.CommonValidation, "A request body is required")
	}
	// The slug is the stable identifier organisations are assigned by, so
	// changing it would leave existing assignments pointing at a tier whose
	// meaning silently changed. The domain has no field for it at all; this
	// says so in the contract's own words.
	if in.Slug != nil {
		return nil, httpx.ErrCodeDetail(errcode.CommonValidation, "slug cannot be changed")
	}
	patch := tiers.Patch{Name: in.Name, Description: in.Description, AllowAllModels: in.AllowAllModels}
	if in.Status != nil {
		status := string(*in.Status)
		patch.Status = &status
	}
	tier, err := s.tiers.Update(ctx, req.TierId, patch)
	if err != nil {
		return nil, tierHTTPError(err)
	}
	return UpdateGatewayTier200JSONResponse(tierDTO(tier)), nil
}

func (s *Server) DeleteGatewayTier(ctx context.Context, req DeleteGatewayTierRequestObject) (DeleteGatewayTierResponseObject, error) {
	if err := s.tiers.Delete(ctx, req.TierId); err != nil {
		return nil, tierHTTPError(err)
	}
	return DeleteGatewayTier204Response{}, nil
}

func (s *Server) SetDefaultGatewayTier(ctx context.Context, req SetDefaultGatewayTierRequestObject) (SetDefaultGatewayTierResponseObject, error) {
	tier, err := s.tiers.SetDefault(ctx, req.TierId)
	if err != nil {
		return nil, tierHTTPError(err)
	}
	return SetDefaultGatewayTier200JSONResponse(tierDTO(tier)), nil
}

func (s *Server) ListGatewayTierModels(ctx context.Context, req ListGatewayTierModelsRequestObject) (ListGatewayTierModelsResponseObject, error) {
	models, err := s.tiers.Models(ctx, req.TierId)
	if err != nil {
		return nil, tierHTTPError(err)
	}
	return ListGatewayTierModels200JSONResponse{Items: tierModelsDTO(models)}, nil
}

func (s *Server) SetGatewayTierModels(ctx context.Context, req SetGatewayTierModelsRequestObject) (SetGatewayTierModelsResponseObject, error) {
	if req.Body == nil {
		return nil, httpx.ErrCodeDetail(errcode.CommonValidation, "model_ids is required")
	}
	models, err := s.tiers.SetModels(ctx, req.TierId, req.Body.ModelIds)
	if err != nil {
		return nil, tierHTTPError(err)
	}
	return SetGatewayTierModels200JSONResponse{Items: tierModelsDTO(models)}, nil
}
