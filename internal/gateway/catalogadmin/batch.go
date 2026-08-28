package catalogadmin

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/fairlb/fairlb/foundation/db"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
	"github.com/fairlb/fairlb/internal/gateway/routeprobe"
)

// Whole-set save for "which upstream models does this provider serve".
//
// A client-side loop of one request per row is fine at eight rows, but "select
// everything after discovery" is bounded by the discovery entry cap of 500 -- a
// hundred serial round trips plus whatever partial state a failure halfway
// through leaves behind is not the right shape for this action.
//
// Three rules, each with a failure mode behind it:
//
//   - Create before delete. The two orders fail asymmetrically: delete-first
//     followed by failing creates can leave a model with no routes at all (a
//     hard 503), while create-first followed by failing deletes leaves an old
//     alias alive a little longer and routing splits some traffic to it by
//     priority and weight. Partially wrong beats entirely wrong. Held in the
//     client, that ordering survives only as long as the tab stays open; here
//     it is guaranteed by the structure of the code.
//   - One failed row does not take the others with it. In Postgres any failing
//     statement voids the whole transaction, so each row runs on its own
//     savepoint (a nested Begin in pgx is a SAVEPOINT). Rows that succeed
//     commit together; a row that fails rolls back only itself.
//   - A unique violation on create and a missing row on delete both count as
//     "already", not "failed": the database is already in the requested state.
//     Calling those failures makes people retry a goal they have reached.

// BatchKind says which half of the set an item belongs to.
type BatchKind string

const (
	BatchCreateKind BatchKind = "create"
	BatchDeleteKind BatchKind = "delete"
)

// BatchOutcome is one row's verdict.
type BatchOutcome string

const (
	// OutcomeDone means the row was applied.
	OutcomeDone BatchOutcome = "done"
	// OutcomeAlready means the database was already in the requested state.
	OutcomeAlready BatchOutcome = "already"
	// OutcomeFailed means this row did not apply; the others may still have.
	OutcomeFailed BatchOutcome = "failed"
)

// NewModel is a catalog entry created along the way, for an upstream name the
// local catalog does not have.
type NewModel struct {
	Slug        string
	DisplayName string
	// ContextWindow and MaxOutputTokens are what the row's suggestion carried,
	// zero when nothing supplied them. They are here so that a model created
	// from the wiring editor is as complete as one created from the catalog
	// page: without them every discovered model arrived with a zero window and
	// had to be finished one at a time on its own page.
	ContextWindow   int32
	MaxOutputTokens *int32
	// OutputModalities is what the row's suggestion said this model produces,
	// empty when nothing did. Empty means text, the column's own default: only
	// the seeded catalog knows this, and an upstream name says nothing about
	// whether the bytes coming back are words or pixels (ADR-0226).
	OutputModalities []string
}

// BatchCreate is one row to wire. Exactly one of ModelID and NewModel is set:
// with both there is no right answer when they disagree, with neither the row
// has nowhere to land.
type BatchCreate struct {
	ModelID         *uuid.UUID
	NewModel        *NewModel
	ProviderModelID string
}

// BatchDelete is one row to remove.
type BatchDelete struct {
	ModelID uuid.UUID
	RouteID uuid.UUID
}

// BatchResult is one row's outcome. Err is set only when Outcome is
// OutcomeFailed, and carries the domain's own error -- narrowing it for a
// client is the transport's job, because a batch endpoint states its verdicts
// inside the body and the problem renderer never sees them.
type BatchResult struct {
	Kind            BatchKind
	ModelID         *uuid.UUID
	RouteID         *uuid.UUID
	ProviderModelID string
	Outcome         BatchOutcome
	Err             error
}

// defaultMaxOutputTokens is what a model created along the way gets, the same
// value single-model creation uses.
const defaultMaxOutputTokens = 4096

// BatchWire applies a whole set of route creations and deletions.
func (s *Service) BatchWire(
	ctx context.Context, providerID uuid.UUID, creates []BatchCreate, deletes []BatchDelete,
) ([]BatchResult, error) {
	// A missing provider is a request-level error, not a per-row one. Otherwise
	// every row repeats the same "model or provider not found" and the reader
	// has to get through fifty of them to learn there was only one problem.
	if _, err := s.q.GetProviderForAdmin(ctx, pgID(providerID)); err != nil {
		if db.IsNoRows(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("catalogadmin: query provider: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("catalogadmin: begin batch wiring transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	results := make([]BatchResult, 0, len(creates)+len(deletes))
	for _, c := range creates {
		results = append(results, s.batchCreateOne(ctx, tx, providerID, c))
	}
	for _, d := range deletes {
		results = append(results, s.batchDeleteOne(ctx, tx, d))
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("catalogadmin: commit batch wiring: %w", err)
	}
	// Invalidate regardless of the per-row outcomes: if any row moved, the
	// cache is stale.
	s.invalidate(ctx)
	return results, nil
}

func failed(out BatchResult, err error) BatchResult {
	out.Outcome = OutcomeFailed
	out.Err = err
	return out
}

func (s *Service) batchCreateOne(
	ctx context.Context, tx pgx.Tx, providerID uuid.UUID, c BatchCreate,
) BatchResult {
	out := BatchResult{
		Kind: BatchCreateKind, ModelID: c.ModelID, ProviderModelID: c.ProviderModelID,
	}
	if (c.ModelID == nil) == (c.NewModel == nil) {
		return failed(out, invalid("Give exactly one of model_id or new_model"))
	}
	sp, err := tx.Begin(ctx)
	if err != nil {
		return failed(out, fmt.Errorf("open savepoint: %w", err))
	}
	defer func() { _ = sp.Rollback(ctx) }()
	qsp := s.q.WithTx(sp)

	modelID := uuid.UUID{}
	if c.ModelID != nil {
		modelID = *c.ModelID
	}
	if c.NewModel != nil {
		// This has to happen after the savepoint is opened. Creating the model
		// and then failing to create the route would leave an empty model
		// nobody asked for, holding a slug that cannot be changed, so the next
		// retry cannot even reuse the name. What makes it safe is that the
		// statement runs after the savepoint opened, not which Tx object it was
		// handed: ROLLBACK TO SAVEPOINT undoes every change made on that
		// session since. Moving these lines above the Begin turns the rollback
		// test red on the spot.
		created, cErr := createModelForBatch(ctx, qsp, *c.NewModel)
		if cErr != nil {
			return failed(out, cErr)
		}
		modelID = created
		out.ModelID = &created
	}

	// The existence check is the same one single-route creation applies. It
	// runs on the savepoint because the outer transaction already holds a
	// connection, and asking the pool for a second one is, on a small pool, a
	// deadlock against yourself.
	providerProtocols, err := routeParties(ctx, qsp, modelID, providerID)
	if err != nil {
		return failed(out, err)
	}

	row, err := qsp.CreateRoute(ctx, gwdb.CreateRouteParams{
		ModelID: pgID(modelID), ProviderID: pgID(providerID),
		ProviderModelID: c.ProviderModelID,
		// Attributes take their defaults: whole-set editing decides membership,
		// not attributes. Priority, weight, headers and limits are changed
		// afterwards through inline editing.
		Priority: 100, Weight: 1, Enabled: true,
		Headers:       encodeMap[string](nil),
		Quirks:        encodeMap[any](nil),
		VideoEnvelope: encodeMap[any](nil),
	})
	if err != nil {
		if db.IsUniqueViolation(err) {
			// This (model, provider, upstream name) is already in the database,
			// which is exactly what was asked for.
			out.Outcome = OutcomeAlready
			return out
		}
		return failed(out, fmt.Errorf("create route: %w", err))
	}
	// The probe goes on the savepoint rather than the outer transaction, so
	// that if this row rolls back, its probe job rolls back with it.
	if err := routeprobe.Enqueue(ctx, qsp, s.river, sp, row.ID, providerProtocols); err != nil {
		return failed(out, err)
	}
	if err := sp.Commit(ctx); err != nil {
		return failed(out, fmt.Errorf("release savepoint: %w", err))
	}
	id := uuid.UUID(row.ID.Bytes)
	out.RouteID = &id
	out.Outcome = OutcomeDone
	return out
}

func (s *Service) batchDeleteOne(ctx context.Context, tx pgx.Tx, d BatchDelete) BatchResult {
	modelID, routeID := d.ModelID, d.RouteID
	out := BatchResult{Kind: BatchDeleteKind, ModelID: &modelID, RouteID: &routeID}

	sp, err := tx.Begin(ctx)
	if err != nil {
		return failed(out, fmt.Errorf("open savepoint: %w", err))
	}
	defer func() { _ = sp.Rollback(ctx) }()

	// Read the upstream name before deleting: the result carries it back so the
	// caller can match the row in its own table by the (model_id,
	// provider_model_id) pair that identifies it.
	//
	// The two-column match mirrors the single-route delete endpoint: a
	// cross-model id matches zero rows and counts as "already", so the caller
	// cannot tell "it exists but is not yours" from "it is not there".
	var upstream string
	err = sp.QueryRow(ctx,
		`SELECT provider_model_id FROM model_routes WHERE id=$1 AND model_id=$2 FOR UPDATE`,
		pgID(routeID), pgID(modelID)).Scan(&upstream)
	if db.IsNoRows(err) {
		out.Outcome = OutcomeAlready
		return out
	}
	if err != nil {
		return failed(out, fmt.Errorf("lock route: %w", err))
	}
	out.ProviderModelID = upstream

	n, err := s.q.WithTx(sp).DeleteRoute(ctx, gwdb.DeleteRouteParams{
		ID: pgID(routeID), ModelID: pgID(modelID),
	})
	if err != nil {
		return failed(out, fmt.Errorf("delete route: %w", err))
	}
	if n == 0 {
		out.Outcome = OutcomeAlready
		return out
	}
	if err := sp.Commit(ctx); err != nil {
		return failed(out, fmt.Errorf("release savepoint: %w", err))
	}
	out.Outcome = OutcomeDone
	return out
}

// createModelForBatch creates the catalog entry along the way.
//
// It always creates the model disabled, exactly as single-model creation does:
// the readiness gate remains the only one. What this relaxes is where a row that
// upstream has and the local catalog does not can land -- not the conditions for
// enabling it. The wiring becomes real; being sellable does not.
//
// A slug collision fails rather than reusing the model of the same name. Same
// name does not imply same model -- two relays can call different things
// `gpt-4o` -- so silently reusing it would attach the route to the wrong
// catalog entry. When a guess cannot be made confidently, ask rather than
// settle for something approximately right.
func createModelForBatch(ctx context.Context, q *gwdb.Queries, in NewModel) (uuid.UUID, error) {
	slug := strings.TrimSpace(in.Slug)
	if slug == "" {
		return uuid.UUID{}, invalid("new_model.slug is required")
	}
	maxOut := int32(defaultMaxOutputTokens)
	if in.MaxOutputTokens != nil {
		maxOut = *in.MaxOutputTokens
	}
	if err := checkModalities(in.OutputModalities); err != nil {
		return uuid.UUID{}, err
	}
	row, err := q.CreateModel(ctx, gwdb.CreateModelParams{
		Slug: slug, DisplayName: in.DisplayName,
		Enabled: false, Visibility: "public",
		ContextWindow: in.ContextWindow, MaxOutputTokens: maxOut,
		OutputModalities: in.OutputModalities,
	})
	if err != nil {
		if err := slugShapeRefusal(err); err != nil {
			return uuid.UUID{}, err
		}
		if db.IsUniqueViolation(err) {
			return uuid.UUID{}, ConflictError{Message: "The slug \"" + slug +
				"\" is already taken. Choose a different name, or select the existing " +
				"model from the catalog instead. Reusing it automatically could attach " +
				"this route to a different model, since the same name does not imply " +
				"the same model."}
		}
		return uuid.UUID{}, fmt.Errorf("create catalog entry: %w", err)
	}
	return uuid.UUID(row.ID.Bytes), nil
}
