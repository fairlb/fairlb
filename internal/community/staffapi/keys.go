package staffapi

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/access/apikeys"
	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/foundation/pgconv"
	"github.com/fairlb/fairlb/foundation/publicid"
)

// The API key management surface.
//
// # What is shared and what is not
//
// The decision logic and the key material itself come from the shared apikeys
// package: generation, hashing, limit validation and invalidating the cache on
// revocation are all one copy of the code. Only two things differ, and both are
// in this layer:
//
//   - where the organisation comes from: from a path parameter in a
//     multi-tenant deployment, and from the request's team here -- or, when it
//     names none, from the team created at first start;
//   - who may manage keys: from membership roles elsewhere, and simply "logged
//     in" here, via the injected always-permitting check.
//
// # The actor id is always the zero value
//
// Community records the administrator action in its audit log. Deployments
// with end-user creators attach that relationship through the key service's
// creator port and keep it in their own schema.

func apiKeyView(k apikeys.Key) ApiKey {
	v := ApiKey{
		Id:             publicid.Format(publicid.Key, k.ID),
		TeamId:         publicid.Format(publicid.Org, k.OrgID),
		Name:           k.Name,
		Prefix:         k.Prefix,
		Scopes:         k.Scopes,
		Status:         ApiKeyStatus(k.Status),
		TotalSpentNano: k.TotalSpentNano,
		CreatedAt:      k.CreatedAt,
	}
	if k.SpendLimitNano != nil {
		v.SpendLimitNano = k.SpendLimitNano
	}
	if k.SpendLimitInterval != nil {
		interval := ApiKeySpendLimitInterval(*k.SpendLimitInterval)
		v.SpendLimitInterval = &interval
	}
	if k.RateLimitRpm != nil {
		rpm := int(*k.RateLimitRpm)
		v.RateLimitRpm = &rpm
	}
	if k.RateLimitTpm != nil {
		tpm := int(*k.RateLimitTpm)
		v.RateLimitTpm = &tpm
	}
	// The column is NOT NULL, so the nil guard is defensive rather than
	// expected -- but a nil slice would render as `null` where the
	// specification promises an array.
	v.ModelAccess = &ModelAccess{AllowAll: k.ModelAccess.AllowAll, Models: k.ModelAccess.Models}
	if len(k.Tags) > 0 {
		v.Tags = &k.Tags
	}
	v.LastUsedAt = k.LastUsedAt
	v.ExpiresAt = k.ExpiresAt
	return v
}

func (s *Server) CommunityListKeys(ctx context.Context, req CommunityListKeysRequestObject) (CommunityListKeysResponseObject, error) {
	lim := int32(50)
	if req.Params.Limit != nil {
		lim = int32(*req.Params.Limit) //nolint:gosec // the spec bounds this to 1..200
	}
	team, err := s.teamOf(ctx, req.Params.TeamId)
	if err != nil {
		return nil, err
	}
	// No cursor pagination: a single instance has keys in the tens, so a cap of
	// 200 is an order of magnitude of headroom. If the scale ever demands
	// paging, there should first be a user journey that explains it.
	rows, err := s.keys.List(ctx, team, pgtype.UUID{}, lim, pgtype.Timestamptz{}, pgtype.UUID{})
	if err != nil {
		return nil, err
	}
	items := make([]ApiKey, 0, len(rows))
	for _, k := range rows {
		items = append(items, apiKeyView(k))
	}
	return CommunityListKeys200JSONResponse{Items: items}, nil
}

func (s *Server) CommunityCreateKey(ctx context.Context, req CommunityCreateKeyRequestObject) (CommunityCreateKeyResponseObject, error) {
	if req.Body == nil {
		return nil, httpx.ErrCode(errcode.CommonValidation)
	}
	team, err := s.teamOf(ctx, req.Body.TeamId)
	if err != nil {
		return nil, err
	}
	in := apikeys.CreateInput{OrgID: team, Name: req.Body.Name}
	if req.Body.ExpiresAt != nil {
		in.ExpiresAt = pgtype.Timestamptz{Time: *req.Body.ExpiresAt, Valid: true}
	}
	if req.Body.SpendLimitNano != nil {
		in.SpendLimitNano = pgtype.Int8{Int64: *req.Body.SpendLimitNano, Valid: true}
	}
	if req.Body.SpendLimitInterval != nil {
		in.SpendLimitInterval = pgtype.Text{String: string(*req.Body.SpendLimitInterval), Valid: true}
	}
	if req.Body.RateLimitRpm != nil {
		in.RateLimitRpm = pgtype.Int4{Int32: int32(*req.Body.RateLimitRpm), Valid: true} //nolint:gosec // spec minimum is 1 and the column CHECKs > 0
	}
	if req.Body.RateLimitTpm != nil {
		in.RateLimitTpm = pgtype.Int4{Int32: int32(*req.Body.RateLimitTpm), Valid: true} //nolint:gosec // spec minimum is 1 and the column CHECKs > 0
	}
	in.ModelAccess = apikeys.UnrestrictedModelAccess()
	if req.Body.ModelAccess != nil {
		in.ModelAccess = modelAccessOf(*req.Body.ModelAccess)
	}
	if req.Body.Tags != nil {
		in.Tags = pgconv.JSONOrNil(*req.Body.Tags)
	}
	// Scopes are not taken from the request: only inference keys are issued
	// here, and the specification has no field for it either.
	plaintext, row, err := s.keys.Create(ctx, in)
	if err != nil {
		return nil, err
	}
	return CommunityCreateKey201JSONResponse{Key: plaintext, ApiKey: apiKeyView(row)}, nil
}

func (s *Server) CommunityUpdateKey(ctx context.Context, req CommunityUpdateKeyRequestObject) (CommunityUpdateKeyResponseObject, error) {
	keyID, err := publicid.Parse(publicid.Key, req.KeyId)
	if err != nil {
		return nil, httpx.ErrCode(errcode.CommonNotFound)
	}
	if req.Body == nil {
		return nil, httpx.ErrCode(errcode.CommonValidation)
	}
	// The key's own team, not the default one: an operator editing a key in a
	// second team must not have their edit refused for belonging to a different
	// organisation than the one this endpoint assumed.
	team, err := s.teamOfKey(ctx, keyID)
	if err != nil {
		return nil, err
	}
	in := apikeys.UpdateInput{OrgID: team, KeyID: keyID}
	if req.Body.Clear != nil {
		for _, c := range *req.Body.Clear {
			switch c {
			case SpendLimit:
				in.ClearSpendLimit = true
			case RateLimitRpm:
				in.ClearRateLimitRpm = true
			case RateLimitTpm:
				in.ClearRateLimitTpm = true
			case ExpiresAt:
				in.ClearExpires = true
			}
		}
	}
	in.SpendLimitNano = req.Body.SpendLimitNano
	if req.Body.SpendLimitInterval != nil {
		iv := string(*req.Body.SpendLimitInterval)
		in.SpendLimitInterval = &iv
	}
	if req.Body.RateLimitRpm != nil {
		rpm := int32(*req.Body.RateLimitRpm) //nolint:gosec // spec minimum is 1 and the column CHECKs > 0
		in.RateLimitRpm = &rpm
	}
	if req.Body.RateLimitTpm != nil {
		tpm := int32(*req.Body.RateLimitTpm) //nolint:gosec // spec minimum is 1 and the column CHECKs > 0
		in.RateLimitTpm = &tpm
	}
	in.ExpiresAt = req.Body.ExpiresAt
	if req.Body.ModelAccess != nil {
		access := modelAccessOf(*req.Body.ModelAccess)
		in.ModelAccess = &access
	}
	if req.Body.Tags != nil {
		in.Tags = pgconv.JSONOrNil(*req.Body.Tags)
	}
	row, err := s.keys.Update(ctx, in)
	if err != nil {
		return nil, err
	}
	return CommunityUpdateKey200JSONResponse(apiKeyView(row)), nil
}

// teamOfKey finds which team a key belongs to.
//
// The shared key service scopes every write by organisation -- one forgotten
// predicate there is a cross-organization leak elsewhere, so the scoping stays -- and
// this deployment addresses keys by id alone, without a team in the path. So
// the team is looked up rather than assumed. Assuming the default one would
// make every edit to a key in a second team answer "not found", which reads as
// "that key does not exist" and is a lie.
func (s *Server) teamOfKey(ctx context.Context, keyID pgtype.UUID) (pgtype.UUID, error) {
	org, err := s.community.GetKeyTeam(ctx, keyID)
	if err != nil {
		return pgtype.UUID{}, httpx.ErrCode(errcode.CommonNotFound)
	}
	return org, nil
}

// modelAccessOf converts the request shape into the service's. The two are
// separate types on purpose: this one is generated from the specification and
// changes when the wire format does, and the service's is what the database
// stores.
func modelAccessOf(in ModelAccess) apikeys.ModelAccess {
	return apikeys.ModelAccess{AllowAll: in.AllowAll, Models: in.Models}
}

func (s *Server) CommunityRevokeKey(ctx context.Context, req CommunityRevokeKeyRequestObject) (CommunityRevokeKeyResponseObject, error) {
	keyID, err := publicid.Parse(publicid.Key, req.KeyId)
	if err != nil {
		return nil, httpx.ErrCode(errcode.CommonNotFound)
	}
	team, err := s.teamOfKey(ctx, keyID)
	if err != nil {
		return nil, err
	}
	if err := s.keys.Revoke(ctx, team, pgtype.UUID{}, keyID); err != nil {
		return nil, err
	}
	return CommunityRevokeKey204Response{}, nil
}
