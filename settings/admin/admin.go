// Package admin is the write path the operator surfaces share: the settings
// endpoint of Cloud's staff plane and of Community's admin plane call the same
// Writer, so "what a write is checked against, what it records, when it becomes
// visible" has one implementation rather than one per product (ADR-0198).
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fairlb/fairlb/audit"
	"github.com/fairlb/fairlb/foundation/crypto"
	"github.com/fairlb/fairlb/foundation/db"
	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/foundation/publicid"
	"github.com/fairlb/fairlb/settings"
)

// Writer applies operator writes: registry checks, the reason policy for
// high-impact keys, one transaction for the batch and its audit rows, then
// cache invalidation after commit.
type Writer struct {
	pool  *pgxpool.Pool
	store *settings.Store
}

func New(pool *pgxpool.Pool, store *settings.Store) *Writer {
	return &Writer{pool: pool, store: store}
}

// Actor is who is writing, as the audit row records it.
type Actor struct {
	Type      string // an audit actor type the table accepts; both operator consoles use "staff"
	ID        pgtype.UUID
	IP        string
	RequestID string
}

// Put writes a batch. An unknown key is a 404; a value off its Spec is a 400;
// a section rule violation is a 422 naming the section; a high-impact key
// without a reason is a 422 before anything is written.
//
// The reason is enforced here and not only in the interface: any caller that
// is not that page — a script, another client — could otherwise change a
// high-impact key with no "why" in the audit log.
func (w *Writer) Put(ctx context.Context, actor Actor, changes []settings.Change, reason string) error {
	if len(changes) == 0 {
		return httpx.ErrCodeDetail(errcode.CommonValidation, "No changes were given")
	}
	reg := w.store.Registry()
	for _, c := range changes {
		spec, ok := reg.Lookup(c.Key)
		if !ok {
			return httpx.ErrCode(errcode.CommonNotFound)
		}
		// Shape before policy: an invalid value answers 400, not "you gave no
		// reason" — the latter points the caller at the wrong fix.
		if err := spec.Validate(c.Value); err != nil {
			return httpx.ErrCodeDetail(errcode.CommonValidation, c.Key+": "+err.Error())
		}
		if spec.Impact == settings.ImpactHigh && reason == "" {
			return httpx.ErrCodeDetail(errcode.CommonSettingsReasonRequired,
				"Changing "+c.Key+" requires a reason; it goes into the audit record")
		}
	}
	by := publicid.UUIDString(actor.ID)
	var written []string
	err := db.WithSystemTx(ctx, w.pool, func(tx pgx.Tx) error {
		keys, err := w.store.Apply(ctx, tx, changes, by)
		if err != nil {
			return MapError(err)
		}
		written = keys
		for _, c := range changes {
			if err := audit.InsertTx(ctx, tx, audit.Entry{
				ActorType: actor.Type, ActorID: actor.ID,
				Action: "settings.update", TargetType: "setting", TargetID: c.Key,
				Meta: auditMeta(reg, c, reason),
				IP:   actor.IP, RequestID: actor.RequestID,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	// Invalidate after commit, so another instance cannot refill its cache
	// from state that is not yet visible.
	for _, key := range written {
		w.store.Invalidate(ctx, key)
	}
	// And once more shortly after (cache-aside's classic hole): a reader that
	// ran its SELECT before the commit but its cache write after the first
	// invalidation would otherwise re-seat the old value for a whole TTL — on
	// a shared cache, for every instance. A second delete a little later is
	// the cheapest closure of that window; the TTL still bounds the rest.
	time.AfterFunc(invalidateAgainAfter, func() {
		bg := context.WithoutCancel(ctx)
		for _, key := range written {
			w.store.Invalidate(bg, key)
		}
	})
	httpx.MarkAudited(ctx)
	return nil
}

// invalidateAgainAfter is how long after the commit the second invalidation
// runs: longer than any in-flight read-through could take, far shorter than
// the cache TTL.
const invalidateAgainAfter = time.Second

// auditMeta is one key's audit meta. A secret is recorded by its mask only:
// the audit table is not a second copy of the credential, and it is read far
// more widely than the settings table.
func auditMeta(reg *settings.Registry, c settings.Change, reason string) map[string]any {
	meta := map[string]any{}
	if spec, _ := reg.Lookup(c.Key); spec.Kind == settings.KindSecret {
		var plain string
		_ = json.Unmarshal(c.Value, &plain)
		if plain == "" {
			meta["cleared"] = true
		} else {
			meta["hint"] = crypto.MaskSecret(plain)
		}
	} else {
		meta["value"] = json.RawMessage(c.Value)
	}
	if reason != "" {
		meta["reason"] = reason
	}
	return meta
}

// MapError turns the store's errors into the contract's codes.
func MapError(err error) error {
	var ve *settings.ValidationError
	var se *settings.SectionError
	switch {
	case errors.Is(err, settings.ErrUnknownKey):
		return httpx.ErrCode(errcode.CommonNotFound)
	case errors.As(err, &ve):
		return httpx.ErrCodeDetail(errcode.CommonValidation, ve.Error())
	case errors.As(err, &se):
		return httpx.ErrCodeDetail(errcode.CommonSettingsSectionInvalid, se.Error())
	default:
		return fmt.Errorf("settings: apply: %w", err)
	}
}
