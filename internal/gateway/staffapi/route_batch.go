package gwstaffapi

import (
	"context"
	"errors"

	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/internal/gateway/catalogadmin"
)

// Whole-set save for "which upstream models does this provider serve".
//
// The rules — create before delete, one savepoint per row, "already" is not a
// failure — live in internal/gateway/catalogadmin (ADR-0178). What is left here
// is the contract, and one part of it is worth stating: a batch endpoint states
// its verdicts **inside the body**, so the problem renderer never sees them and
// this file has to do its own narrowing. An error carrying a code passes its
// code and detail through; anything else collapses to the generic internal code
// with no text, because the original may contain a connection string, table
// names or constraint names.

func batchItemDTO(r catalogadmin.BatchResult) GatewayRouteBatchItemResult {
	out := GatewayRouteBatchItemResult{
		Kind:            GatewayRouteBatchItemResultKind(r.Kind),
		Outcome:         GatewayRouteBatchItemResultOutcome(r.Outcome),
		ModelId:         r.ModelID,
		RouteId:         r.RouteID,
		ProviderModelId: r.ProviderModelID,
	}
	if r.Err == nil {
		return out
	}
	var invalid catalogadmin.InvalidError
	var conflict catalogadmin.ConflictError
	switch {
	case errors.As(r.Err, &invalid):
		code, detail := errcode.CommonValidation, invalid.Message
		out.Code, out.Detail = &code, &detail
	case errors.As(r.Err, &conflict):
		code, detail := errcode.CommonConflict, conflict.Message
		out.Code, out.Detail = &code, &detail
	case errors.Is(r.Err, catalogadmin.ErrNotFound):
		code, detail := errcode.CommonNotFound, "Model or provider not found"
		out.Code, out.Detail = &code, &detail
	default:
		code := errcode.CommonInternal
		out.Code = &code
	}
	return out
}

func (s *Server) BatchWireProviderRoutes(
	ctx context.Context, req BatchWireProviderRoutesRequestObject,
) (BatchWireProviderRoutesResponseObject, error) {
	in := req.Body
	if in == nil {
		return nil, httpx.ErrCodeDetail(errcode.CommonValidation, "A request body is required")
	}
	creates := make([]catalogadmin.BatchCreate, 0, len(in.Creates))
	for _, c := range in.Creates {
		item := catalogadmin.BatchCreate{
			ModelID:         c.ModelId,
			ProviderModelID: c.ProviderModelId,
		}
		if c.NewModel != nil {
			item.NewModel = &catalogadmin.NewModel{
				Slug:        c.NewModel.Slug,
				DisplayName: derefOr(c.NewModel.DisplayName, ""),
			}
		}
		creates = append(creates, item)
	}
	deletes := make([]catalogadmin.BatchDelete, 0, len(in.Deletes))
	for _, d := range in.Deletes {
		deletes = append(deletes, catalogadmin.BatchDelete{ModelID: d.ModelId, RouteID: d.RouteId})
	}

	results, err := s.catalogAdmin.BatchWire(ctx, req.ProviderId, creates, deletes)
	if err != nil {
		if errors.Is(err, catalogadmin.ErrNotFound) {
			return nil, httpx.ErrCodeDetail(errcode.CommonNotFound, "Provider not found")
		}
		return nil, err
	}
	out := make([]GatewayRouteBatchItemResult, 0, len(results))
	for _, r := range results {
		out = append(out, batchItemDTO(r))
	}
	return BatchWireProviderRoutes200JSONResponse{Results: out}, nil
}
