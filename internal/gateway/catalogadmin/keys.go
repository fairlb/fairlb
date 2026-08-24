package catalogadmin

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/foundation/crypto"
	"github.com/fairlb/fairlb/foundation/cursorpage"
	"github.com/fairlb/fairlb/foundation/db"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
)

// Provider credentials.
//
// One rule governs the whole file: **an undecryptable credential is worse than
// no credential at all**, because routing keeps treating it as a usable
// candidate and keeps failing on it. That is why creating a key writes the row,
// seals against its id, and deletes the row if either step after the insert
// fails.

// ProviderKey is one credential, never carrying the secret itself.
type ProviderKey struct {
	ID         uuid.UUID
	ProviderID uuid.UUID
	Name       string
	Status     string
	// SecretHint keeps a recognizable head and tail so two keys can be told
	// apart on screen.
	SecretHint     string
	LastVerifiedAt time.Time
	LastError      string
	// CreatedAt is the pagination key's leading component; it is not rendered.
	CreatedAt time.Time
	// CooldownUntil comes from the same source the routing layer reads:
	// cooldowns(scope='provider_key'). A second source would let the screen and
	// the router disagree about whether a key is in cooldown.
	CooldownUntil time.Time
}

// KeyPatch changes a credential's name or status; nil leaves it alone.
type KeyPatch struct {
	Name   *string
	Status *string
}

// duplicateKeyName is the message for the collision that surprises people:
// names are unique per provider, and two unnamed keys both have the empty name,
// so the second one collides while the operator's experience is "but I did not
// type a name at all".
const duplicateKeyName = "This provider already has a key with that name; an empty name " +
	"counts as a duplicate, so give this key a name"

func timeOf(v pgtype.Timestamptz) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return v.Time.UTC()
}

// ProviderKeys lists a provider's credentials, oldest first — the same order the
// connectivity test picks from, so the first row of the first page stays the key
// it has always used.
func (s *Service) ProviderKeys(
	ctx context.Context, providerID uuid.UUID, page cursorpage.Page,
) ([]ProviderKey, error) {
	rows, err := s.q.ListProviderKeysForAdmin(ctx, gwdb.ListProviderKeysForAdminParams{
		ProviderID:      pgID(providerID),
		CursorCreatedAt: page.CursorAt, CursorID: page.CursorID,
		Lim: page.ProbeLimit(),
	})
	if err != nil {
		return nil, fmt.Errorf("catalogadmin: list provider keys: %w", err)
	}
	out := make([]ProviderKey, 0, len(rows))
	for _, r := range rows {
		out = append(out, ProviderKey{
			ID: uuid.UUID(r.ID.Bytes), ProviderID: uuid.UUID(r.ProviderID.Bytes),
			Name: r.Name, Status: r.Status, SecretHint: r.SecretHint,
			LastVerifiedAt: timeOf(r.LastVerifiedAt), LastError: r.LastError,
			CooldownUntil: timeOf(r.CooldownUntil), CreatedAt: timeOf(r.CreatedAt),
		})
	}
	return out, nil
}

// UpdateProviderKey changes a credential's status or name.
//
// The safe rotation order is "add the new key, verify it, then disable the old
// one". With only create and delete available, the sole possible order is
// "delete the old one first" -- the most dangerous one. That is why this exists.
func (s *Service) UpdateProviderKey(
	ctx context.Context, providerID, keyID uuid.UUID, in KeyPatch,
) (ProviderKey, error) {
	params := gwdb.UpdateProviderKeyParams{ID: pgID(keyID), ProviderID: pgID(providerID)}
	if in.Name != nil {
		params.Name = pgtype.Text{String: *in.Name, Valid: true}
	}
	if in.Status != nil {
		params.Status = pgtype.Text{String: *in.Status, Valid: true}
	}
	row, err := s.q.UpdateProviderKey(ctx, params)
	if err != nil {
		if db.IsUniqueViolation(err) {
			return ProviderKey{}, ConflictError{Message: duplicateKeyName}
		}
		// An UPDATE constrained by provider_id matches zero rows when the key
		// belongs to another provider, so the :one query reports no rows.
		// Answering exactly as for "does not exist" is correct: the caller must
		// not be able to tell "it exists but is not yours" from "it is not
		// there".
		return ProviderKey{}, ErrNotFound
	}
	// Disabling a key removes it from the routing candidates (the lookup only
	// takes active ones), so the catalog cache has to be invalidated with it --
	// otherwise the change does not take effect until the TTL expires.
	s.invalidate(ctx)
	return ProviderKey{
		ID: uuid.UUID(row.ID.Bytes), ProviderID: uuid.UUID(row.ProviderID.Bytes),
		Name: row.Name, Status: row.Status, SecretHint: row.SecretHint,
		LastVerifiedAt: timeOf(row.LastVerifiedAt), LastError: row.LastError,
	}, nil
}

// CreateProviderKey stores a credential.
//
// The row is inserted first to get its id: the ciphertext's AAD binds to that
// id, so the id has to exist before the secret can be sealed. Writing in two
// steps is deliberate -- the AAD binding is what stops a ciphertext from being
// copied onto another row and reused there.
func (s *Service) CreateProviderKey(
	ctx context.Context, box *crypto.Box, providerID uuid.UUID, name, secret string,
) (ProviderKey, error) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return ProviderKey{}, invalid("secret is required")
	}
	// Whether this is the provider's first credential is read before the
	// insert: it decides below whether the provider's routes, deferred by
	// the probe worker for want of a key, are now probed. A later key changes
	// nothing about what the routes serve and must not re-buy every verdict.
	active, err := s.q.CountActiveProviderKeys(ctx, pgID(providerID))
	if err != nil {
		return ProviderKey{}, fmt.Errorf("catalogadmin: count provider keys: %w", err)
	}
	row, err := s.q.CreateProviderKey(ctx, gwdb.CreateProviderKeyParams{
		ProviderID: pgID(providerID), Name: name,
		SecretEnc:  []byte{}, // placeholder; filled in below once the id is known
		SecretHint: crypto.MaskSecret(secret),
	})
	if err != nil {
		if db.IsUniqueViolation(err) {
			return ProviderKey{}, ConflictError{Message: duplicateKeyName}
		}
		return ProviderKey{}, fmt.Errorf("catalogadmin: create provider key: %w", err)
	}
	drop := func() {
		_, _ = s.q.DeleteProviderKey(ctx, gwdb.DeleteProviderKeyParams{
			ID: row.ID, ProviderID: pgID(providerID),
		})
	}
	enc, err := box.Seal([]byte(secret), row.ID.Bytes[:])
	if err != nil {
		drop()
		return ProviderKey{}, fmt.Errorf("catalogadmin: encrypt provider key: %w", err)
	}
	if err := s.q.SetProviderKeySecret(ctx, gwdb.SetProviderKeySecretParams{
		ID: row.ID, SecretEnc: enc,
	}); err != nil {
		drop()
		return ProviderKey{}, fmt.Errorf("catalogadmin: write ciphertext: %w", err)
	}
	// Routes created before the provider had a credential were deferred by
	// the probe worker; the first credential is the moment they can be
	// answered.
	if active == 0 {
		s.probes.EnqueueProviderRoutes(ctx, pgID(providerID))
	}
	s.invalidate(ctx)
	return ProviderKey{
		ID: uuid.UUID(row.ID.Bytes), ProviderID: uuid.UUID(row.ProviderID.Bytes),
		Name: row.Name, Status: row.Status, SecretHint: row.SecretHint,
	}, nil
}

// DeleteProviderKey removes a credential.
func (s *Service) DeleteProviderKey(ctx context.Context, providerID, keyID uuid.UUID) error {
	n, err := s.q.DeleteProviderKey(ctx, gwdb.DeleteProviderKeyParams{
		ID: pgID(keyID), ProviderID: pgID(providerID),
	})
	if err != nil {
		return fmt.Errorf("catalogadmin: delete provider key: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	s.invalidate(ctx)
	return nil
}

// VerifiedProviderKeys counts the credentials that have verified, over the whole
// set rather than over a page.
//
// The readiness checklist needs one boolean, and deriving it from a page would
// answer "none verified" whenever the verified key sits further down — a step
// shown incomplete while the thing it checks is done.
func (s *Service) VerifiedProviderKeys(ctx context.Context, providerID uuid.UUID) (int64, error) {
	n, err := s.q.CountVerifiedProviderKeys(ctx, pgID(providerID))
	if err != nil {
		return 0, fmt.Errorf("catalogadmin: count verified provider keys: %w", err)
	}
	return n, nil
}

// ProviderKeyCursor points just past k.
func ProviderKeyCursor(k ProviderKey) string {
	return cursorpage.Encode(k.CreatedAt, k.ID.String())
}
