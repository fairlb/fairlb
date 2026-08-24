// Package apikeys manages virtual API keys. The key format lives in the keyfmt
// package; the plaintext is returned exactly once, at creation, and only a
// SHA-256 of it plus a display prefix are stored.
//
// Enforcing budgets and rate limits, and broadcasting cache invalidations, are
// the data plane's job. This package provides the management surface and the
// query primitives that authentication and budget checks are built from.
package apikeys

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fairlb/fairlb/access/keyfmt"
	"github.com/fairlb/fairlb/access/organizations"
	"github.com/fairlb/fairlb/access/orgstatus"
	"github.com/fairlb/fairlb/foundation/crypto"
	"github.com/fairlb/fairlb/foundation/db"
	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/foundation/publicid"
)

// Invalidator is called with a key's hash when that key stops being valid. The
// data plane caches a key-to-org snapshot under that hash, so a revocation has
// to punch through the cache immediately: otherwise a leaked key keeps spending
// until the cache entry expires. This package does not know about that cache, so
// the callback is injected at the assembly point.
type Invalidator func(ctx context.Context, keyHash string)

// CreatorRecorder lets a deployment attach its own identity relationship to a
// newly-created public key without adding deployment-owned columns to api_keys.
// It runs in the same transaction as the key insert.
type CreatorRecorder func(context.Context, pgx.Tx, pgtype.UUID, pgtype.UUID) error

// KeyAdmin decides whether a user subject may manage this org's keys, returning
// a CodeError when they may not.
//
// This is the one piece of the package whose answer depends on the deployment
// shape. A multi-tenant deployment decides it from org membership — a
// non-member gets 404 so the org's existence stays hidden, a plain member gets
// 403 — while a single-org deployment has no membership table at all and being
// signed in is already the highest privilege. Injecting just this decision lets
// the rest of the package (generation, hashing, limits, revocation, cache
// invalidation) be one implementation serving both.
//
// It is a function type rather than an interface: there is only one method, and
// the multi-tenant implementation needs to close over its own query handle, so a
// closure is more honest than a struct that exists only to carry it.
type KeyAdmin func(ctx context.Context, orgID, actorID pgtype.UUID) error

// ModelAdmission reports which of the given model slugs this org may not reach,
// so a key can never be given access its organization does not have.
//
// It returns the offending slugs rather than a yes/no, because the caller has
// to name them: "one of these is not available to you" sends somebody to
// compare two lists by hand.
//
// It is injected for the same reason KeyAdmin is -- what an org may reach is
// decided by the gateway's catalogue, which sits above this package and must
// not be imported from it. Not supplying it means the check does not run, which
// is safe in the direction that matters: the data plane refuses an unreachable
// model anyway, and this check exists to say so at save time instead of at
// three in the morning.
type ModelAdmission func(ctx context.Context, orgID pgtype.UUID, slugs []string) ([]string, error)

type Service struct {
	pool       *pgxpool.Pool
	store      *Store
	orgs       *organizations.Store
	admin      KeyAdmin
	creator    CreatorRecorder
	invalidate Invalidator
	admission  ModelAdmission
}

// ServiceConfig contains deployment-owned outlets. Supplying them at
// construction keeps Service immutable after it becomes reachable by HTTP
// handlers.
type ServiceConfig struct {
	Database       *pgxpool.Pool
	Admin          KeyAdmin
	Creator        CreatorRecorder
	Invalidator    Invalidator
	ModelAdmission ModelAdmission
}

// NewService builds the key management service. A nil admin denies everything:
// a missing authorization decision must not degrade into permitting, because the
// symptom of a forgotten injection would be "anyone can manage anyone's keys"
// with nothing reporting an error.
func NewService(cfg ServiceConfig) *Service {
	if cfg.Admin == nil {
		cfg.Admin = func(context.Context, pgtype.UUID, pgtype.UUID) error {
			return httpx.ErrCode(errcode.CommonForbidden)
		}
	}
	return &Service{
		pool: cfg.Database, store: NewStore(cfg.Database), orgs: organizations.New(cfg.Database), admin: cfg.Admin,
		creator: cfg.Creator, invalidate: cfg.Invalidator,
		admission: cfg.ModelAdmission,
	}
}

const (
	displayHeadLen = 8 // the display form is the fixed prefix plus this many characters
	maxNameLen     = 100
)

// requireAdmin checks the org binding locally for credential subjects and hands
// user subjects to the injected KeyAdmin.
//
// The credential branch stays here rather than also being injected because it
// does not vary with the deployment shape: a management key's authorization is
// decided by the API layer's scope gate — endpoint allowlist, scope, org binding
// — identically in both. Pushing it out would make each implementation copy the
// same branch, and the copy that got it wrong would silently permit.
func (s *Service) requireAdmin(ctx context.Context, orgID, userID pgtype.UUID) error {
	// A management key is not a user and has no membership to look up.
	// Re-checking the org here is defense in depth rather than duplication: if
	// some future call path brings a credential subject in another way, a
	// mismatched org still does not get through.
	if p := httpx.PrincipalFrom(ctx); p.IsCredential() {
		if p.OrgID != uuidStr(orgID) {
			return httpx.ErrCode(errcode.CommonNotFound)
		}
		return nil
	}
	return s.admin(ctx, orgID, userID)
}

// requireOrgActive refuses key writes when the org's status makes it
// non-writable: suspended is 403, pending deletion is 409. The decision itself
// lives in the orgstatus package so every enforcement point agrees. Read paths
// are not gated.
func (s *Service) requireOrgActive(ctx context.Context, orgID pgtype.UUID) error {
	status, err := s.orgs.Status(ctx, orgID)
	if err != nil {
		return fmt.Errorf("apikeys: read org status: %w", err)
	}
	return orgstatus.RequireWritable(status)
}

// checkModelAccess refuses an allowlist that reaches past what the org itself
// may reach.
//
// Without it the extra slugs are not an error, just dead entries: the data
// plane resolves them and answers 404, so the key looks configured for a model
// it can never call. Catching it here turns a silent misconfiguration into a
// message at the moment somebody typed it.
func (s *Service) checkModelAccess(ctx context.Context, orgID pgtype.UUID, access ModelAccess) error {
	if s.admission == nil || access.AllowAll || len(access.Models) == 0 {
		return nil
	}
	rejected, err := s.admission(ctx, orgID, access.Models)
	if err != nil {
		return fmt.Errorf("apikeys: check model admission: %w", err)
	}
	if len(rejected) > 0 {
		return httpx.ErrCodeDetail(errcode.CommonValidation,
			"These models are not available to this organization: "+strings.Join(rejected, ", "))
	}
	return nil
}

// CreateInput is a key creation request. Every limit is optional; the data
// plane is what gives them meaning.
type CreateInput struct {
	OrgID, ActorID     pgtype.UUID
	Name               string
	ExpiresAt          pgtype.Timestamptz
	SpendLimitNano     pgtype.Int8
	SpendLimitInterval pgtype.Text
	RateLimitRpm       pgtype.Int4
	RateLimitTpm       pgtype.Int4
	// ModelAccess says which models the key may call. The zero value is
	// "restricted to nothing", which is never what a caller that did not think
	// about it means, so Create reads the zero value as unrestricted -- see
	// ModelAccess.normalise.
	ModelAccess ModelAccess
	Tags        []byte   // jsonb array of strings; nil means no tags
	Scopes      []string // empty falls back to the column default
}

// ModelAccess is a key's own model gate: everything, or exactly this list.
//
// It is a pair rather than a list because "no models at all" and "every model"
// both have to be sayable, and one list cannot say both -- whichever meaning
// emptiness is given, the other becomes inexpressible. Reading emptiness as
// "everything" is the direction that fails open, so the intent is carried
// explicitly.
type ModelAccess struct {
	AllowAll bool
	Models   []string
}

// Key is the stable application read model returned to API planes. Database
// nullability and jsonb decoding are normalized here so every plane cannot
// independently reinterpret the same row.
type Key struct {
	ID, OrgID          pgtype.UUID
	Name, Prefix       string
	Scopes             []string
	Status             string
	SpendLimitNano     *int64
	SpendLimitInterval *string
	RateLimitRpm       *int32
	RateLimitTpm       *int32
	ModelAccess        ModelAccess
	Tags               []string
	TotalSpentNano     int64
	CreatedAt          time.Time
	LastUsedAt         *time.Time
	ExpiresAt          *time.Time
}

func keyFromRow(row Record) Key {
	models := row.AllowedModels
	if models == nil {
		models = []string{}
	}
	var tags []string
	if len(row.Tags) > 0 && json.Unmarshal(row.Tags, &tags) != nil {
		tags = nil
	}
	key := Key{
		ID: row.ID, OrgID: row.OrgID, Name: row.Name, Prefix: row.Prefix,
		Scopes: row.Scopes, Status: row.Status,
		ModelAccess: ModelAccess{AllowAll: row.AllowAllModels, Models: models},
		Tags:        tags, TotalSpentNano: row.TotalSpentNano, CreatedAt: row.CreatedAt.Time,
	}
	if row.SpendLimitNano.Valid {
		value := row.SpendLimitNano.Int64
		key.SpendLimitNano = &value
	}
	if row.SpendLimitInterval.Valid {
		value := row.SpendLimitInterval.String
		key.SpendLimitInterval = &value
	}
	if row.RateLimitRpm.Valid {
		value := row.RateLimitRpm.Int32
		key.RateLimitRpm = &value
	}
	if row.RateLimitTpm.Valid {
		value := row.RateLimitTpm.Int32
		key.RateLimitTpm = &value
	}
	if row.LastUsedAt.Valid {
		value := row.LastUsedAt.Time
		key.LastUsedAt = &value
	}
	if row.ExpiresAt.Valid {
		value := row.ExpiresAt.Time
		key.ExpiresAt = &value
	}
	return key
}

// UnrestrictedModelAccess is the default for a new key: the key adds no gate of
// its own and reaches whatever its organization's admission tier allows.
func UnrestrictedModelAccess() ModelAccess { return ModelAccess{AllowAll: true} }

// normalise makes the stored pair match the CHECK on the table: with AllowAll
// the list is empty, so a non-empty list in the database always means a
// restricted key and nobody has to read two columns to know which.
//
// It also guarantees a non-nil slice, because the column is NOT NULL and a nil
// slice would be sent as NULL.
func (m ModelAccess) normalise() ModelAccess {
	if m.AllowAll || len(m.Models) == 0 {
		return ModelAccess{AllowAll: m.AllowAll, Models: []string{}}
	}
	return m
}

// UpdateInput changes a key's control fields, with three states per field: nil
// keeps the current value, a Clear* flag removes it, anything else sets it.
//
// "Not supplied" and "cleared" have to be expressible separately. With a single
// nullable value, "remove the spend limit" and "I am not touching the spend
// limit this time" are the same request body on the wire.
type UpdateInput struct {
	OrgID, ActorID, KeyID pgtype.UUID
	SpendLimitNano        *int64
	SpendLimitInterval    *string
	RateLimitRpm          *int32
	RateLimitTpm          *int32
	ExpiresAt             *time.Time
	ClearSpendLimit       bool
	ClearRateLimitRpm     bool
	ClearRateLimitTpm     bool
	ClearExpires          bool
	// ModelAccess replaces the key's model gate when non-nil. A pointer,
	// because an empty allowlist is a real value here -- it refuses every
	// model -- so "sent an empty list" must not arrive as "sent nothing".
	ModelAccess *ModelAccess
	Tags        []byte
}

// Create makes a key. The plaintext is exposed exactly once, through the return
// value; only its SHA-256 and the display prefix are stored.
var uuidStr = publicid.UUIDString

// inOrgTx runs inside the org scope. Row-level security is the outer layer,
// stacked on top of the explicit org_id predicates in the queries themselves.
// One forgotten predicate is a cross-organization leak, and this is what catches it.
func (s *Service) inOrgTx(ctx context.Context, orgID pgtype.UUID, fn func(store *Store) error) error {
	return db.WithOrgTx(ctx, s.pool, uuidStr(orgID), func(tx pgx.Tx) error {
		return fn(s.store.WithTx(tx))
	})
}

func (s *Service) Create(ctx context.Context, in CreateInput) (string, Key, error) {
	if err := s.requireAdmin(ctx, in.OrgID, in.ActorID); err != nil {
		return "", Key{}, err
	}
	if err := s.requireOrgActive(ctx, in.OrgID); err != nil {
		return "", Key{}, err
	}
	name := strings.TrimSpace(in.Name)
	if name == "" || len(name) > maxNameLen {
		return "", Key{}, httpx.ErrCodeDetail(errcode.CommonValidation, "A name is required and must be at most 100 characters")
	}
	plaintext, err := newKeyPlaintext()
	if err != nil {
		return "", Key{}, fmt.Errorf("apikeys: generate key: %w", err)
	}
	// A management key may only mint inference keys, never another management
	// key: that would be privilege escalation. Adding a management key requires
	// a person in the console, where there is a session, a membership role and
	// an audit trail.
	scopes := in.Scopes
	if httpx.PrincipalFrom(ctx).IsCredential() {
		scopes = []string{ScopeInference}
	}
	// A caller that set nothing gets an unrestricted key, not one that refuses
	// every model: the zero value of a struct is not a decision, and reading it
	// as the strictest possible one would make every key created by a caller
	// that has not been updated yet answer 404 for everything.
	access := in.ModelAccess.normalise()
	if !access.AllowAll && len(access.Models) == 0 {
		access = UnrestrictedModelAccess().normalise()
	}
	if err := s.checkModelAccess(ctx, in.OrgID, access); err != nil {
		return "", Key{}, err
	}
	var row Record
	err = db.WithOrgTx(ctx, s.pool, uuidStr(in.OrgID), func(tx pgx.Tx) error {
		store := s.store.WithTx(tx)
		var iErr error
		row, iErr = store.Insert(ctx, InsertParams{
			OrgID:              in.OrgID,
			Name:               name,
			Prefix:             plaintext[:len(keyfmt.Prefix)+displayHeadLen],
			KeyHash:            crypto.HashToken(plaintext),
			SpendLimitNano:     in.SpendLimitNano,
			SpendLimitInterval: in.SpendLimitInterval,
			RateLimitRpm:       in.RateLimitRpm,
			RateLimitTpm:       in.RateLimitTpm,
			ExpiresAt:          in.ExpiresAt,
			AllowAllModels:     access.AllowAll,
			AllowedModels:      access.Models,
			Tags:               in.Tags,
			Scopes:             scopes,
		})
		if iErr != nil {
			return iErr
		}
		if s.creator != nil && !httpx.PrincipalFrom(ctx).IsCredential() && in.ActorID.Valid {
			return s.creator(ctx, tx, row.ID, in.ActorID)
		}
		return nil
	})
	if db.IsUniqueViolation(err) {
		return "", Key{}, httpx.ErrCodeDetail(errcode.CommonValidation, "A key with this name already exists")
	}
	if err != nil {
		return "", Key{}, fmt.Errorf("apikeys: store key: %w", err)
	}
	return plaintext, keyFromRow(row), nil
}

// List returns every key of an org, including revoked ones so the history is
// auditable. The plaintext can never be reproduced.
func (s *Service) List(ctx context.Context, orgID, actorID pgtype.UUID, limit int32, curTS pgtype.Timestamptz, curID pgtype.UUID) ([]Key, error) {
	if err := s.requireAdmin(ctx, orgID, actorID); err != nil {
		return nil, err
	}
	var rows []Record
	if err := s.inOrgTx(ctx, orgID, func(store *Store) error {
		var lErr error
		rows, lErr = store.ListRecordsByOrg(ctx, orgID, limit, curTS, curID)
		return lErr
	}); err != nil {
		return nil, fmt.Errorf("apikeys: list keys: %w", err)
	}
	out := make([]Key, 0, len(rows))
	for _, row := range rows {
		out = append(out, keyFromRow(row))
	}
	return out, nil
}

// Revoke takes effect immediately and is idempotent. A key belonging to another
// org is always 404.
func (s *Service) Revoke(ctx context.Context, orgID, actorID, keyID pgtype.UUID) error {
	if err := s.requireAdmin(ctx, orgID, actorID); err != nil {
		return err
	}
	if err := s.requireOrgActive(ctx, orgID); err != nil {
		return err
	}
	// Fetching the row and revoking it share one org-scoped transaction. That
	// gives the 404 semantics (missing, or another org's), yields the hash
	// needed for the invalidation broadcast, and leaves no window in which a
	// concurrent insert of the same id could separate the two steps.
	var key Record
	err := s.inOrgTx(ctx, orgID, func(store *Store) error {
		var gErr error
		key, gErr = store.RecordByOrg(ctx, keyID, orgID)
		if gErr != nil {
			return gErr
		}
		_, rErr := store.Revoke(ctx, keyID, orgID)
		return rErr
	})
	if err != nil {
		if db.IsNoRows(err) {
			return httpx.ErrCode(errcode.CommonNotFound)
		}
		return fmt.Errorf("apikeys: revoke key: %w", err)
	}
	// Broadcast even on a repeated revocation: invalidation is idempotent, and
	// one extra broadcast is cheaper than a stale cache entry surviving.
	if s.invalidate != nil {
		s.invalidate(ctx, key.KeyHash)
	}
	return nil
}

// Update changes the control fields. It does not rename, does not rotate the
// secret, and does not resurrect a revoked key.
//
// It must broadcast a cache invalidation afterwards: the data plane's cached
// identity carries the model gate and the budgets, so without it a newly set
// allowlist would not apply until the cache expired. These fields are security
// semantics, and "takes effect in a little while" is not acceptable for them.
func (s *Service) Update(ctx context.Context, in UpdateInput) (Key, error) {
	if err := s.requireAdmin(ctx, in.OrgID, in.ActorID); err != nil {
		return Key{}, err
	}
	if err := s.requireOrgActive(ctx, in.OrgID); err != nil {
		return Key{}, err
	}
	if in.SpendLimitNano != nil && in.SpendLimitInterval == nil {
		return Key{}, httpx.ErrCodeDetail(errcode.CommonValidation,
			"A spend limit needs a billing interval as well")
	}

	params := UpdateControlsParams{
		ID: in.KeyID, OrgID: in.OrgID,
		ClearSpendLimit:   in.ClearSpendLimit,
		ClearRateLimitRpm: in.ClearRateLimitRpm,
		ClearRateLimitTpm: in.ClearRateLimitTpm,
		ClearExpires:      in.ClearExpires,
		Tags:              in.Tags,
		// The columns are written only when the caller supplied a decision;
		// otherwise the statement carries the current values forward. The
		// allowlist still has to be a non-nil slice: it is a NOT NULL column,
		// and the parameter is sent whether or not the CASE selects it.
		AllowedModels: []string{},
	}
	if in.ModelAccess != nil {
		access := in.ModelAccess.normalise()
		if err := s.checkModelAccess(ctx, in.OrgID, access); err != nil {
			return Key{}, err
		}
		params.SetModelAccess = true
		params.AllowAllModels = access.AllowAll
		params.AllowedModels = access.Models
	}
	if in.SpendLimitNano != nil {
		params.SpendLimitNano = pgtype.Int8{Int64: *in.SpendLimitNano, Valid: true}
	}
	if in.SpendLimitInterval != nil {
		params.SpendLimitInterval = pgtype.Text{String: *in.SpendLimitInterval, Valid: true}
	}
	if in.RateLimitRpm != nil {
		params.RateLimitRpm = pgtype.Int4{Int32: *in.RateLimitRpm, Valid: true}
	}
	if in.RateLimitTpm != nil {
		params.RateLimitTpm = pgtype.Int4{Int32: *in.RateLimitTpm, Valid: true}
	}
	if in.ExpiresAt != nil {
		params.ExpiresAt = pgtype.Timestamptz{Time: *in.ExpiresAt, Valid: true}
	}

	var row Record
	err := s.inOrgTx(ctx, in.OrgID, func(store *Store) error {
		var uErr error
		row, uErr = store.UpdateControls(ctx, params)
		return uErr
	})
	if err != nil {
		if db.IsNoRows(err) {
			// Missing, another org's, or already revoked: to the caller these
			// are all "there is no such key to change".
			return Key{}, httpx.ErrCode(errcode.CommonNotFound)
		}
		return Key{}, fmt.Errorf("apikeys: update key controls: %w", err)
	}
	if s.invalidate != nil {
		s.invalidate(ctx, row.KeyHash)
	}
	return keyFromRow(row), nil
}

// newKeyPlaintext generates the key plaintext; the format itself lives in the
// keyfmt package.
func newKeyPlaintext() (string, error) {
	return keyfmt.New()
}
