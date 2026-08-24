package tiers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/foundation/db"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
)

// One organization's admission settings: which tier it is on, and its rate
// ceilings.
//
// They live together because they are decided together, and because both of
// them are carried in the data-plane's cached identity -- so both invalidate the
// same thing when they change.

var (
	// ErrTierDisabled is assigning a tier the data plane refuses.
	ErrTierDisabled = errors.New("tiers: that tier is disabled and cannot be assigned")
	// ErrOrgNotFound is an org_id that does not exist.
	ErrOrgNotFound = errors.New("tiers: organization not found")
	// ErrNoDefaultTier means the seeded default tier is gone. It is a server
	// fault, not a caller mistake: the migration seeds one and it can be
	// neither deleted nor disabled, so reaching this state means somebody
	// changed the database around the application.
	ErrNoDefaultTier = errors.New(
		"tiers: there is no default access tier -- the row seeded by the migration was deleted outside the application layer")
)

// OrgSettings is what an organization is admitted under.
type OrgSettings struct {
	TierID             uuid.UUID
	TierSlug           string
	TierName           string
	TierStatus         string
	TierAllowAllModels bool
	// TierExplicit distinguishes "assigned this tier" from "following the
	// default", which look identical once resolved but are different decisions.
	TierExplicit bool
	// RowExists says whether this organization has a settings row at all.
	RowExists    bool
	RateLimitRPM *int32
	RateLimitTPM *int32
}

// OrgSettingsWrite replaces the whole row.
//
// Whole-row rather than partial: these are decided together, and a partial
// update would need a sentinel to express "clear the tier back to the default"
// and another for "remove this ceiling" -- sentinels that buy nothing. An
// omitted ceiling is therefore removed, not kept.
type OrgSettingsWrite struct {
	TierID       *uuid.UUID
	RateLimitRPM *int32
	RateLimitTPM *int32
}

// KeyCacheInvalidator clears the data-plane's cached identity for one key hash.
//
// Declared here and implemented at the assembly point: the cache is downstream
// of admission, and a domain that imported the proxy to name its cache key
// would have the arrow pointing the wrong way.
type KeyCacheInvalidator interface {
	InvalidateKeyHash(ctx context.Context, hash string) error
}

func settingsFrom(r gwdb.GetOrgGatewaySettingsRow) (OrgSettings, error) {
	if !r.TierID.Valid {
		return OrgSettings{}, ErrNoDefaultTier
	}
	out := OrgSettings{
		TierID: uuid.UUID(r.TierID.Bytes), TierSlug: r.TierSlug, TierName: r.TierName,
		TierStatus: r.TierStatus, TierAllowAllModels: r.TierAllowAllModels,
		TierExplicit: r.TierExplicit, RowExists: r.RowExists,
	}
	if r.RateLimitRpm.Valid {
		rpm := r.RateLimitRpm.Int32
		out.RateLimitRPM = &rpm
	}
	if r.RateLimitTpm.Valid {
		tpm := r.RateLimitTpm.Int32
		out.RateLimitTPM = &tpm
	}
	return out, nil
}

// OrgSettings reads one organization's admission settings.
func (s *Service) OrgSettings(ctx context.Context, orgID pgtype.UUID) (OrgSettings, error) {
	row, err := s.q.GetOrgGatewaySettings(ctx, orgID)
	if err != nil {
		return OrgSettings{}, fmt.Errorf("tiers: read organization settings: %w", err)
	}
	return settingsFrom(row)
}

// PutOrgSettings replaces them.
func (s *Service) PutOrgSettings(
	ctx context.Context, orgID pgtype.UUID, in OrgSettingsWrite, keys KeyCacheInvalidator,
) (OrgSettings, error) {
	var tierID pgtype.UUID
	if in.TierID != nil {
		tierID = pgID(*in.TierID)
		// A disabled tier cannot be assigned: the data plane fails closed on
		// one, so assigning it would put the organization in a state where the
		// configuration looks fine and every request is refused.
		tier, err := s.q.GetTier(ctx, tierID)
		if err != nil {
			return OrgSettings{}, ErrNotFound
		}
		if tier.Status != "active" {
			return OrgSettings{}, ErrTierDisabled
		}
	}
	params := gwdb.PutOrgGatewaySettingsParams{OrgID: orgID, TierID: tierID}
	if in.RateLimitRPM != nil {
		params.RateLimitRpm = pgtype.Int4{Int32: *in.RateLimitRPM, Valid: true}
	}
	if in.RateLimitTPM != nil {
		params.RateLimitTpm = pgtype.Int4{Int32: *in.RateLimitTPM, Valid: true}
	}
	if _, err := s.q.PutOrgGatewaySettings(ctx, params); err != nil {
		if db.IsForeignKeyViolation(err) {
			// A nonexistent org hits the foreign key on org_id.
			return OrgSettings{}, ErrOrgNotFound
		}
		return OrgSettings{}, fmt.Errorf("tiers: write organization settings: %w", err)
	}

	// The cached identity carries the tier and both ceilings, so without
	// invalidation there is a window of up to one TTL in which requests are
	// still admitted and metered according to the old values.
	s.invalidateOrgKeys(ctx, orgID, keys)

	row, err := s.q.GetOrgGatewaySettings(ctx, orgID)
	if err != nil {
		return OrgSettings{}, fmt.Errorf("tiers: read back organization settings: %w", err)
	}
	return settingsFrom(row)
}

// invalidateOrgKeys clears the data-plane cache for every active key this org
// owns.
//
// Cache keys are built from the key hash, so "invalidate by org" means looking
// the hashes up first and deleting them one at a time.
//
// A failure here logs and does not fail the request. The write has already
// committed, so returning an error would only make the operator retry and store
// the same value again, while the cache that needed clearing still would not be
// cleared. The best outcome available is to say so loudly and let the TTL cover
// the rest -- a window of at most one cache TTL priced on the old values.
func (s *Service) invalidateOrgKeys(ctx context.Context, orgID pgtype.UUID, keys KeyCacheInvalidator) {
	if keys == nil {
		return // no cache injected: reads go straight to the database
	}
	hashes, err := s.q.ListActiveKeyHashesForOrg(ctx, orgID)
	if err != nil {
		slog.ErrorContext(ctx,
			"failed to look up the organization key hashes; the data-plane cache was not "+
				"invalidated, so the new tier/ceilings are delayed until the TTL expires",
			"org_id", orgID, "error", err)
		return
	}
	for _, h := range hashes {
		if err := keys.InvalidateKeyHash(ctx, h); err != nil {
			slog.ErrorContext(ctx,
				"data-plane key cache invalidation failed; this key is delayed until the TTL expires",
				"org_id", orgID, "error", err)
		}
	}
}

// Admission is what the data plane resolves before it admits a request: which
// tier is in force, whether it still is, and the two ceilings.
type Admission struct {
	TierID     uuid.UUID
	TierStatus string
	// Valid is false when the organization resolves to no tier at all, which is
	// the same refusal as a disabled one: the data plane admits nothing.
	Valid        bool
	RateLimitRPM *int32
	RateLimitTPM *int32
}

// EffectiveAdmission reads it on the caller's own connection.
//
// A package function rather than a Service method because the caller already
// holds the org-scoped transaction: these rows are under row-level security, so
// the read has to be on the connection that set the scope (ADR-0179).
func EffectiveAdmission(ctx context.Context, q *gwdb.Queries, orgID pgtype.UUID) (Admission, error) {
	row, err := q.GetOrgSettingsForDataplane(ctx, orgID)
	if err != nil {
		return Admission{}, fmt.Errorf("tiers: read admission tier: %w", err)
	}
	out := Admission{
		TierID: uuid.UUID(row.TierID.Bytes), TierStatus: row.TierStatus,
		Valid: row.TierID.Valid,
	}
	if row.RateLimitRpm.Valid {
		rpm := row.RateLimitRpm.Int32
		out.RateLimitRPM = &rpm
	}
	if row.RateLimitTpm.Valid {
		tpm := row.RateLimitTpm.Int32
		out.RateLimitTPM = &tpm
	}
	return out, nil
}
