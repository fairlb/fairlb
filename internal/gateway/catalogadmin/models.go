package catalogadmin

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/fairlb/fairlb/foundation/db"
	fdb "github.com/fairlb/fairlb/foundation/db"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
)

// Model is a catalog entry.
type Model struct {
	ID               uuid.UUID
	Slug             string
	DisplayName      string
	Enabled          bool
	Visibility       string
	OutputModalities []string
}

// ModelCreate is a new catalog entry. There is no protocol to state: a model
// is reachable on whatever its routes' providers speak.
type ModelCreate struct {
	Slug        string
	DisplayName string
	Visibility  string
	// OutputModalities is what the model produces. Empty means text, which is
	// what the column defaults to -- a modality is stated only by the models
	// that are not text (ADR-0226).
	OutputModalities []string
	ContextWindow    int32
	MaxOutputTokens  *int32
}

// ModelPatch is a partial update; nil leaves a field alone.
type ModelPatch struct {
	DisplayName      *string
	Enabled          *bool
	Visibility       *string
	OutputModalities []string
	ContextWindow    *int32
	MaxOutputTokens  *int32
}

func modelFrom(r gwdb.CreateModelRow) Model {
	return Model{
		ID: uuid.UUID(r.ID.Bytes), Slug: r.Slug, DisplayName: r.DisplayName,
		Enabled: r.Enabled, Visibility: r.Visibility,
		// Returned rather than echoed back from the request: the column decides
		// what an unset list becomes, and a create that answered with what the
		// caller sent would report an empty list for every text model.
		OutputModalities: r.OutputModalities,
	}
}

// CreateModel adds a catalog entry, always disabled.
//
// A new model has to clear the pricing and provider checks before anyone
// enables it explicitly. That stays fail-closed even if a caller submits
// enabled=true -- the field is simply not read here.
func (s *Service) CreateModel(ctx context.Context, in ModelCreate) (Model, error) {
	if in.Slug == "" {
		return Model{}, invalid("slug is required")
	}
	visibility := in.Visibility
	if visibility == "" {
		visibility = "public"
	}
	// An output cap of 0 would degrade the pre-authorization estimate for
	// requests that omit max_tokens into an input-only estimate, so an absent
	// value takes a conservative default rather than zero.
	maxOut := int32(defaultMaxOutputTokens)
	if in.MaxOutputTokens != nil {
		maxOut = *in.MaxOutputTokens
	}
	if err := checkModalities(in.OutputModalities); err != nil {
		return Model{}, err
	}
	row, err := s.q.CreateModel(ctx, gwdb.CreateModelParams{
		Slug: in.Slug, DisplayName: in.DisplayName,
		Enabled: false, Visibility: visibility,
		ContextWindow: in.ContextWindow, MaxOutputTokens: maxOut,
		OutputModalities: in.OutputModalities,
	})
	if err != nil {
		if db.IsUniqueViolation(err) {
			return Model{}, ConflictError{Message: "That slug is already taken"}
		}
		if err := slugShapeRefusal(err); err != nil {
			return Model{}, err
		}
		return Model{}, fmt.Errorf("catalogadmin: create model: %w", err)
	}
	s.invalidate(ctx)
	return modelFrom(row), nil
}

// UpdateModel applies a partial change, refusing to enable a model that has
// nothing to serve it.
func (s *Service) UpdateModel(ctx context.Context, id uuid.UUID, in ModelPatch) (Model, error) {
	if in.Enabled != nil && *in.Enabled {
		ready, err := s.modelReadyToEnable(ctx, id)
		if err != nil {
			return Model{}, fmt.Errorf("catalogadmin: check the preconditions for enabling a model: %w", err)
		}
		if !ready {
			return Model{}, ConflictError{Message: "Publish complete pricing and configure at least one " +
				"enabled provider route before enabling this model"}
		}
	}
	if err := checkModalities(in.OutputModalities); err != nil {
		return Model{}, err
	}
	row, err := s.q.UpdateModel(ctx, gwdb.UpdateModelParams{
		ID:               pgID(id),
		DisplayName:      textOrNull(in.DisplayName),
		Enabled:          boolOrNull(in.Enabled),
		Visibility:       textOrNull(in.Visibility),
		OutputModalities: in.OutputModalities,
		ContextWindow:    int4(in.ContextWindow),
		MaxOutputTokens:  int4(in.MaxOutputTokens),
	})
	if err != nil {
		if db.IsNoRows(err) {
			return Model{}, ErrNotFound
		}
		return Model{}, fmt.Errorf("catalogadmin: update model: %w", err)
	}
	s.invalidate(ctx)
	return modelFrom(gwdb.CreateModelRow(row)), nil
}

// modelReadyToEnable is the server-side gate behind the readiness checklist on
// the model page. A hint in the UI is not a boundary: calling the API directly
// must not be able to enable a model that has no price or nothing to serve it.
func (s *Service) modelReadyToEnable(ctx context.Context, modelID uuid.UUID) (bool, error) {
	var ready bool
	err := s.pool.QueryRow(ctx, `
SELECT
  EXISTS (
    SELECT 1 FROM model_pricing mp
    WHERE mp.model_id = $1
      AND (
        mp.billing_mode = 'free'
        OR (
          mp.upstream_in_nano_per_mtok IS NOT NULL
          AND mp.upstream_out_nano_per_mtok IS NOT NULL
          AND mp.upstream_cache_read_nano_per_mtok IS NOT NULL
          AND mp.upstream_cache_write_nano_per_mtok IS NOT NULL
          AND (mp.upstream_in_nano_per_mtok <> 0 OR mp.upstream_out_nano_per_mtok <> 0
               OR mp.upstream_cache_read_nano_per_mtok <> 0 OR mp.upstream_cache_write_nano_per_mtok <> 0)
        )
      )
  )
  AND EXISTS (
    SELECT 1 FROM model_routes r
    JOIN providers p ON p.id = r.provider_id
    WHERE r.model_id = $1 AND r.enabled AND p.enabled
  )`, pgID(modelID)).Scan(&ready)
	return ready, err
}

// AdminModel is a catalog entry as the operator console lists it: the model's
// own columns plus the number of routes configured on it.
//
// Wider than Model, which is what a write returns. Keeping them separate keeps
// the write path from having to invent counts it did not read.
type AdminModel struct {
	ID              uuid.UUID
	Slug            string
	DisplayName     string
	Enabled         bool
	Visibility      string
	ContextWindow   int32
	MaxOutputTokens int32
	// OutputModalities is what this model produces (ADR-0226). It is the
	// catalog filter's modality axis, and the edit dialog prefills from it.
	OutputModalities []string
	// Metadata is configuration and display only. Malformed jsonb reads as
	// empty rather than failing: one bad row must not make the page unopenable.
	Metadata map[string]any
	// Endpoints is what probes have verified on the model's enabled routes --
	// the same set the public catalog publishes.
	Endpoints []string
	// Protocols is the configuration view: every protocol spoken by a provider
	// with an enabled route for this model. Wider than the published set,
	// which only covers verified endpoints; this one drives the catalog
	// filter, where "configured on anthropic" is the question.
	Protocols  []string
	RouteCount int64
}

// AdminModels lists the catalog, filtered by search.
//
// Searchable but **not paginated**, and the order is the point (ADR-0187): four
// of this list's consumers are whole-set editors, which need every configured
// model to stay visible and ticked. Search gives them somewhere to move to; the
// cursor comes after they have moved, not before.
func (s *Service) AdminModels(ctx context.Context, search string) ([]AdminModel, error) {
	rows, err := s.q.ListModelsForAdmin(ctx, fdb.SearchTerm(search))
	if err != nil {
		return nil, fmt.Errorf("catalogadmin: list models: %w", err)
	}
	out := make([]AdminModel, 0, len(rows))
	for _, r := range rows {
		out = append(out, adminModelFrom(r))
	}
	return out, nil
}

// AdminModel reads one entry.
//
// Like the provider detail view, it reads its own row rather than picking an
// entry out of the capped list.
func (s *Service) AdminModel(ctx context.Context, id uuid.UUID) (AdminModel, error) {
	row, err := s.q.GetModelForAdmin(ctx, pgID(id))
	if err != nil {
		return AdminModel{}, ErrNotFound
	}
	// The two queries return identical columns, so this converts the struct
	// instead of writing a second mapping: the moment they diverge, this stops
	// compiling.
	return adminModelFrom(gwdb.ListModelsForAdminRow(row)), nil
}

func adminModelFrom(r gwdb.ListModelsForAdminRow) AdminModel {
	return AdminModel{
		ID: r.ID.Bytes, Slug: r.Slug, DisplayName: r.DisplayName,
		Enabled: r.Enabled, Visibility: r.Visibility,
		ContextWindow: r.ContextWindow, MaxOutputTokens: r.MaxOutputTokens,
		OutputModalities: r.OutputModalities,
		Metadata:         decodeJSONObject(r.Metadata), Endpoints: catalog.PublishedEndpoints(r.Endpoints),
		Protocols: r.Protocols, RouteCount: r.RouteCount,
	}
}

// slugShapeRefusal turns the database's refusal of a malformed slug into a
// message about the slug, or returns nil when the error is something else.
//
// The pattern itself lives in the migration and is not repeated here. A copy
// in Go would be a second thing to keep in step, and the one that matters is
// the one the database applies to every writer -- including a migration, a
// psql session, and any future code path that forgets to ask.
func slugShapeRefusal(err error) error {
	if !db.IsCheckViolation(err) || db.ConstraintName(err) != "models_slug_shape" {
		return nil
	}
	return invalid("A model slug is written as <creator>/<name>, for example " +
		"\"openai/gpt-5.6-sol\": lowercase, and the part before the slash is who " +
		"made the model rather than which provider serves it. A bare upstream " +
		"name is not a slug -- the same name means different models on different " +
		"upstreams, and a slug cannot be changed once it exists.")
}

// checkModalities refuses a modality the column would refuse anyway, so the
// answer names the field instead of surfacing as a constraint violation.
//
// An empty list is not an error here: it means "unchanged" on the update path
// and "text" on the create path, and both are decided in SQL so that the two
// cannot answer differently.
func checkModalities(v []string) error {
	for _, m := range v {
		if !catalog.ValidModality(m) {
			return invalid(
				"output_modalities: %q is not a modality; expected one of text, image, video", m)
		}
	}
	return nil
}
