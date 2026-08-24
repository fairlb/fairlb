// Package tiers owns admission tiers: which models an organization may reach.
//
// A tier is the gateway's answer to "is this organization allowed to call this
// model", so every rule here is about **refusing**, and every one of them has a
// direction: when in doubt, admit less. That is why a new tier restricts by
// default, why the default tier cannot be disabled or deleted, and why a failed
// edit must never leave a tier wider than it was.
package tiers

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fairlb/fairlb/foundation/cursorpage"
	"github.com/fairlb/fairlb/foundation/db"
	fdb "github.com/fairlb/fairlb/foundation/db"
	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
)

var (
	// ErrNotFound is "no such tier".
	ErrNotFound = errors.New("tiers: not found")
	// ErrSlugTaken is a create that collides with an existing slug.
	ErrSlugTaken = errors.New("tiers: slug already taken")
	// ErrDefaultProtected is an attempt to disable or delete the default tier.
	// It is where an organization with no explicit tier lands, so removing it
	// refuses service to every one of them.
	ErrDefaultProtected = errors.New("tiers: the default tier is protected")
	// ErrNotSettableAsDefault is a disabled or missing tier being made default.
	ErrNotSettableAsDefault = errors.New("tiers: this tier cannot be made the default")
	// ErrAllowAllConflict is listing models on a tier that admits everything.
	ErrAllowAllConflict = errors.New("tiers: a tier that admits every model cannot also list models")
	// ErrUnknownModel is a model id that does not exist.
	ErrUnknownModel = errors.New("tiers: unknown model")
)

// MembersError is a delete refused because organizations are still on the tier.
// It carries the count, because "move them first" is only actionable if you know
// how many there are.
type MembersError struct{ Count int64 }

func (e MembersError) Error() string {
	return fmt.Sprintf("tiers: %d organizations are still on this tier", e.Count)
}

// Tier is an admission tier.
type Tier struct {
	ID          uuid.UUID
	Slug        string
	Name        string
	Description string
	// AllowAllModels and a non-empty model list are mutually exclusive: a list
	// that is carried but never consulted would make a tier read as restricted
	// when it is not.
	AllowAllModels bool
	IsDefault      bool
	Status         string
	// ModelCount and OrgCount are only filled by List: recomputing those
	// subqueries in the response to a single write is of no value to a caller
	// that just submitted the change itself.
	ModelCount int64
	OrgCount   int64
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Model is one model admitted by a tier.
type Model struct {
	ID          uuid.UUID
	Slug        string
	DisplayName string
	Enabled     bool
	Visibility  string
}

// Create is a new tier.
type Create struct {
	Slug           string
	Name           string
	Description    string
	AllowAllModels bool
}

// Patch is a partial update; a nil field is left alone. The slug is absent on
// purpose: it is the stable identifier organizations are assigned by, so
// changing it would leave existing assignments pointing at a tier whose meaning
// silently changed.
type Patch struct {
	Name           *string
	Description    *string
	Status         *string
	AllowAllModels *bool
}

// Invalidator is told the admission set changed. Declared here, implemented at
// the assembly point: the catalog read cache is downstream of admission.
type Invalidator interface {
	InvalidateAll(ctx context.Context)
}

// Service reads and writes tiers.
type Service struct {
	pool        *pgxpool.Pool
	q           *gwdb.Queries
	invalidator Invalidator
}

func NewService(pool *pgxpool.Pool, invalidator Invalidator) *Service {
	return &Service{pool: pool, q: gwdb.New(pool), invalidator: invalidator}
}

func (s *Service) invalidate(ctx context.Context) {
	if s.invalidator != nil {
		s.invalidator.InvalidateAll(ctx)
	}
}

func tierFrom(r gwdb.ModelTier) Tier {
	return Tier{
		ID: uuid.UUID(r.ID.Bytes), Slug: r.Slug, Name: r.Name, Description: r.Description,
		AllowAllModels: r.AllowAllModels, IsDefault: r.IsDefault, Status: r.Status,
		CreatedAt: r.CreatedAt.Time.UTC(), UpdatedAt: r.UpdatedAt.Time.UTC(),
	}
}

// List returns a page of tiers, with how many models and organizations each holds.
//
// Keyed on (is_default, slug): the default tier stays first, everything else
// sorts by slug, and the cursor follows that same key so paging never rearranges
// the list to suit itself (ADR-0191).
func (s *Service) List(ctx context.Context, page cursorpage.KeyPage, search string) ([]Tier, error) {
	cursorIsDefault, err := page.BoolAt(0)
	if err != nil {
		return nil, httpx.ErrCodeDetail(errcode.CommonValidation, "Invalid cursor")
	}
	rows, err := s.q.ListTiers(ctx, gwdb.ListTiersParams{
		HasCursor:       page.HasKey(),
		CursorIsDefault: cursorIsDefault,
		CursorSlug:      page.At(1),
		Search:          fdb.SearchTerm(search),
		Lim:             page.ProbeLimit(),
	})
	if err != nil {
		return nil, fmt.Errorf("tiers: list: %w", err)
	}
	out := make([]Tier, 0, len(rows))
	for _, r := range rows {
		out = append(out, Tier{
			ID: uuid.UUID(r.ID.Bytes), Slug: r.Slug, Name: r.Name, Description: r.Description,
			AllowAllModels: r.AllowAllModels, IsDefault: r.IsDefault, Status: r.Status,
			ModelCount: r.ModelCount, OrgCount: r.OrgCount,
			CreatedAt: r.CreatedAt.Time.UTC(), UpdatedAt: r.UpdatedAt.Time.UTC(),
		})
	}
	return out, nil
}

// Create adds a tier.
//
// A new tier restricts by default. Creating one is an act of restricting, and a
// tier that admitted everything until somebody remembered to narrow it would be
// permissive in exactly the window nobody is watching.
func (s *Service) Create(ctx context.Context, in Create) (Tier, error) {
	row, err := s.q.CreateTier(ctx, gwdb.CreateTierParams{
		Slug: in.Slug, Name: in.Name, Description: in.Description,
		AllowAllModels: in.AllowAllModels,
	})
	if err != nil {
		if db.IsUniqueViolation(err) {
			return Tier{}, ErrSlugTaken
		}
		return Tier{}, fmt.Errorf("tiers: create: %w", err)
	}
	return tierFrom(row), nil
}

// Update applies a partial change.
func (s *Service) Update(ctx context.Context, id uuid.UUID, in Patch) (Tier, error) {
	var row gwdb.ModelTier
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		var uErr error
		row, uErr = q.UpdateTier(ctx, gwdb.UpdateTierParams{
			ID:             pgID(id),
			Name:           textOrNull(in.Name),
			Description:    textOrNull(in.Description),
			Status:         textOrNull(in.Status),
			AllowAllModels: boolOrNull(in.AllowAllModels),
		})
		if uErr != nil {
			return uErr
		}
		// Same transaction, so the pair never rests in the state the schema
		// forbids for a key: admitting everything while still listing models.
		// The list would be carried and never consulted, and somebody reading
		// the table would conclude the tier is restricted.
		if row.AllowAllModels {
			return q.ClearTierModels(ctx, row.ID)
		}
		return nil
	})
	if err != nil {
		if db.IsNoRows(err) {
			return Tier{}, ErrNotFound
		}
		// A database constraint enforces that the default tier stays active.
		if db.IsCheckViolation(err) {
			return Tier{}, ErrDefaultProtected
		}
		return Tier{}, fmt.Errorf("tiers: update: %w", err)
	}
	// What a tier admits is what the catalogue reads, so a change has to reach
	// the read cache -- including a rename, which used not to invalidate at all.
	s.invalidate(ctx)
	return tierFrom(row), nil
}

// Delete removes a tier, refusing two cases: the default tier, and one that
// still has members.
//
// The second does not rely on the raw ON DELETE RESTRICT error, because that
// message does not say how many members are left. Whoever is doing this needs
// to know how many organizations they have to move, not just that it cannot be
// deleted.
func (s *Service) Delete(ctx context.Context, id uuid.UUID) error {
	n, err := s.q.CountOrgsOnTier(ctx, pgID(id))
	if err != nil {
		return fmt.Errorf("tiers: count members: %w", err)
	}
	if n > 0 {
		return MembersError{Count: n}
	}
	deleted, err := s.q.DeleteTier(ctx, pgID(id))
	if err != nil {
		return fmt.Errorf("tiers: delete: %w", err)
	}
	if deleted == 0 {
		// The query carries a NOT is_default predicate, so zero rows means
		// either "no such tier" or "it is the default". Reporting them
		// separately is what keeps the operator from concluding they clicked
		// the wrong row.
		if t, gErr := s.q.GetTier(ctx, pgID(id)); gErr == nil && t.IsDefault {
			return ErrDefaultProtected
		}
		return ErrNotFound
	}
	s.invalidate(ctx)
	return nil
}

// SetDefault moves the default tier.
//
// Clearing the old one and setting the new one share a transaction: the
// single-default unique constraint takes effect within the statement, so the
// intermediate state of one UPDATE cannot pass it, while doing it as two
// requests opens a window with no default at all -- and during that window an
// organization without an explicit tier has nowhere to fall back to.
func (s *Service) SetDefault(ctx context.Context, id uuid.UUID) (Tier, error) {
	var row gwdb.ModelTier
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		if err := q.ClearDefaultTier(ctx); err != nil {
			return err
		}
		// The predicate requires an active tier: making a disabled one the
		// default refuses service to every organization without an explicit
		// tier.
		n, err := q.MarkDefaultTier(ctx, pgID(id))
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotSettableAsDefault
		}
		row, err = q.GetTier(ctx, pgID(id))
		return err
	})
	if err != nil {
		if errors.Is(err, ErrNotSettableAsDefault) {
			return Tier{}, ErrNotSettableAsDefault
		}
		return Tier{}, fmt.Errorf("tiers: set default: %w", err)
	}
	s.invalidate(ctx)
	return tierFrom(row), nil
}

// Models lists the models a tier admits.
func (s *Service) Models(ctx context.Context, id uuid.UUID) ([]Model, error) {
	if _, err := s.q.GetTier(ctx, pgID(id)); err != nil {
		return nil, ErrNotFound
	}
	rows, err := s.q.ListTierModels(ctx, pgID(id))
	if err != nil {
		return nil, fmt.Errorf("tiers: list models: %w", err)
	}
	return modelsFrom(rows), nil
}

// SetModels replaces the tier's model set wholesale.
//
// Clear and insert share one transaction. A failure in between that left
// "cleared but not inserted" behind would turn the tier from "limited to N
// models" into "unrestricted" -- turning a failed edit into a grant of access.
func (s *Service) SetModels(ctx context.Context, id uuid.UUID, modelIDs []uuid.UUID) ([]Model, error) {
	tier, err := s.q.GetTier(ctx, pgID(id))
	if err != nil {
		return nil, ErrNotFound
	}
	ids := dedupe(modelIDs)
	// Listing models on a tier that admits everything is a contradiction, and
	// storing it would produce exactly the pair the schema forbids elsewhere.
	// Refusing says which of the two the caller has to change; silently
	// dropping either half would leave them looking at a screen that does not
	// match what they submitted.
	if tier.AllowAllModels && len(ids) > 0 {
		return nil, ErrAllowAllConflict
	}
	var rows []gwdb.ListTierModelsRow
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		if err := q.ClearTierModels(ctx, pgID(id)); err != nil {
			return err
		}
		if len(ids) > 0 {
			if err := q.AddTierModels(ctx, gwdb.AddTierModelsParams{
				TierID: pgID(id), ModelIds: ids,
			}); err != nil {
				return err
			}
		}
		var lErr error
		rows, lErr = q.ListTierModels(ctx, pgID(id))
		return lErr
	})
	if err != nil {
		// A nonexistent model id hits the foreign key. That is a caller
		// mistake, not a server fault.
		if db.IsForeignKeyViolation(err) {
			return nil, ErrUnknownModel
		}
		return nil, fmt.Errorf("tiers: set models: %w", err)
	}
	// The admission set changed, so the catalog read cache goes wholesale:
	// which models are visible to whom changed with it.
	s.invalidate(ctx)
	return modelsFrom(rows), nil
}

func modelsFrom(rows []gwdb.ListTierModelsRow) []Model {
	out := make([]Model, 0, len(rows))
	for _, m := range rows {
		out = append(out, Model{
			ID: uuid.UUID(m.ID.Bytes), Slug: m.Slug, DisplayName: m.DisplayName,
			Enabled: m.Enabled, Visibility: m.Visibility,
		})
	}
	return out
}

func pgID(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }

func textOrNull(v *string) pgtype.Text {
	if v == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *v, Valid: true}
}

func boolOrNull(v *bool) pgtype.Bool {
	if v == nil {
		return pgtype.Bool{}
	}
	return pgtype.Bool{Bool: *v, Valid: true}
}

// dedupe removes duplicates. Duplicate ids would be harmless thanks to
// ON CONFLICT DO NOTHING, but deduplicating keeps the returned set matching the
// caller's intent and saves pointless rows.
func dedupe(in []uuid.UUID) []pgtype.UUID {
	seen := make(map[uuid.UUID]bool, len(in))
	out := make([]pgtype.UUID, 0, len(in))
	for _, id := range in {
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, pgID(id))
	}
	return out
}

// TierCursor points just past t. Booleans travel as "t"/"f" — the same one-letter
// form PostgreSQL itself prints — so the cursor stays a text tuple.
func TierCursor(t Tier) string {
	return cursorpage.EncodeKey(boolKey(t.IsDefault), t.Slug)
}

// TierCursorParts is the component count the transport hands to ParseKeyPage.
const TierCursorParts = 2

func boolKey(b bool) string {
	if b {
		return "t"
	}
	return "f"
}
