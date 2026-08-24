// Package settings reads and writes runtime settings, with changes taking
// effect without a restart.
//
// Reads go through a read-through cache with a TTL backstop. A write deletes the
// cache entry, which broadcasts an invalidation: this instance drops its copy
// immediately and the others drop theirs on the notification. Keys are
// namespaced per layer, and each layer registers its own — see registry.go.
package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fairlb/fairlb/foundation/crypto"
	"github.com/fairlb/fairlb/foundation/db"
	"github.com/fairlb/fairlb/foundation/drivers/cache"
)

// cacheTTL is the backstop for the read-through cache. The invalidation
// broadcast is the primary path; the TTL only bounds how stale a value can get
// in the corner cases — a lost notification, or the window while a listener
// reconnects.
const cacheTTL = 60 * time.Second

func cacheKey(key string) string { return "settings:" + key }

// ErrUnknownKey is returned for a key the registry does not know.
var ErrUnknownKey = errors.New("settings: unknown key")

// ErrSecretUnreadable is returned when a stored secret cannot be opened: the
// master key changed, or the row was tampered with. Callers treat it as "not
// configured" and log it once — serving on a credential that cannot be read is
// not an option, and neither is failing every request until someone notices.
var ErrSecretUnreadable = errors.New("settings: stored secret cannot be decrypted with the current SECRET_KEY")

// ValidationError is a value that does not fit its key's Spec.
type ValidationError struct {
	Key string
	Err error
}

func (e *ValidationError) Error() string { return e.Key + ": " + e.Err.Error() }
func (e *ValidationError) Unwrap() error { return e.Err }

// Store reads and writes settings: read-through cache, invalidation broadcast
// on write.
//
// Invariant: settings must be written through Store.Set or Store.Apply.
// A direct SQL write to the settings table triggers no invalidation, and every
// instance keeps serving the old value until the TTL expires.
type Store struct {
	db    dbtx
	cache cache.Store // nil reads straight from the database (tests, degraded paths)
	// reg is what this assembled server knows about. It is a field rather than a
	// package global so that "which keys exist" is a property of the server that
	// was built, not of which packages happened to be linked (ADR-0194).
	reg *Registry
	// box seals KindSecret values. The cache only ever holds ciphertext, so a
	// shared cache (Redis) never carries a credential in the clear.
	box *crypto.Box
}

type dbtx interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// New builds a Store. With a nil cache it reads the database every time, which
// is behaviorally identical, just uncached.
//
// A nil registry is a Store that knows no keys: every Lookup misses and the
// settings page is empty. That is the honest reading of "nothing was assembled",
// and it is what the criterion in registry_test.go asserts.
//
// A registry with secret keys needs a Box; building without one panics. The
// alternative — failing at the first write — would let a server run with a
// credentials page it can never save, and that page looks exactly like one that
// nobody has filled in yet.
func New(pool *pgxpool.Pool, c cache.Store, reg *Registry, box *crypto.Box) *Store {
	if reg.HasSecrets() && box == nil {
		panic("settings: the registry has secret keys but no Box was supplied")
	}
	return &Store{db: pool, cache: c, reg: reg, box: box}
}

// Registry is what this Store accepts and renders. The settings endpoint asks it
// before every write: an unregistered key is a 404.
func (s *Store) Registry() *Registry { return s.reg }

// Set stores the raw JSON value of any registered key and invalidates the cache.
// The caller validates the value against the key's Spec first. It runs no
// section rules: use Apply for keys that belong to a section.
func (s *Store) Set(ctx context.Context, key string, value json.RawMessage, updatedBy string) error {
	if err := s.set(ctx, s.db, key, value, updatedBy); err != nil {
		return err
	}
	s.Invalidate(ctx, key)
	return nil
}

// SetTx writes on the caller's transaction. The caller invalidates only after
// commit so another process cannot refill the cache from uncommitted state.
func (s *Store) SetTx(ctx context.Context, tx pgx.Tx, key string, value json.RawMessage, updatedBy string) error {
	return s.set(ctx, tx, key, value, updatedBy)
}

func (s *Store) set(ctx context.Context, d dbtx, key string, value json.RawMessage, updatedBy string) error {
	spec, _ := s.reg.Lookup(key)
	if spec.Kind != KindSecret {
		_, err := d.Exec(ctx, `
			INSERT INTO settings (key, value, updated_by) VALUES ($1, $2, $3)
			ON CONFLICT (key) DO UPDATE SET value = excluded.value, secret_enc = NULL, secret_hint = NULL,
				updated_by = excluded.updated_by, updated_at = now()`,
			key, value, updatedBy)
		return err
	}
	var plaintext string
	if err := json.Unmarshal(value, &plaintext); err != nil {
		return &ValidationError{Key: key, Err: errors.New("must be a string")}
	}
	if plaintext == "" {
		// An empty secret is "clear it". There is no stored form of "set to
		// empty" — a blank credential configures nothing.
		_, err := d.Exec(ctx, `DELETE FROM settings WHERE key = $1`, key)
		return err
	}
	// The AAD is the key: a ciphertext copied onto another key's row fails to
	// open, the same role the row id plays for provider_keys.
	sealed, err := s.box.Seal(value, []byte(key))
	if err != nil {
		return fmt.Errorf("settings: seal %s: %w", key, err)
	}
	_, err = d.Exec(ctx, `
		INSERT INTO settings (key, secret_enc, secret_hint, updated_by) VALUES ($1, $2, $3, $4)
		ON CONFLICT (key) DO UPDATE SET value = NULL, secret_enc = excluded.secret_enc,
			secret_hint = excluded.secret_hint, updated_by = excluded.updated_by, updated_at = now()`,
		key, sealed, crypto.MaskSecret(plaintext), updatedBy)
	return err
}

// Entry is what the generic settings endpoint returns for one key: its
// registration metadata plus the current value.
type Entry struct {
	Spec      Spec
	Value     json.RawMessage // Spec.Default when nothing is stored; nil for a secret
	Set       bool            // whether a value is stored, separating "explicitly set to the default" from "never set"
	Hint      string          // display mask of a stored secret; empty otherwise
	UpdatedAt time.Time
	UpdatedBy string
}

// List returns every registered key with its current value, falling back to
// Spec.Default. Secrets come back with Set and Hint only.
func (s *Store) List(ctx context.Context) ([]Entry, error) {
	rows, err := s.db.Query(ctx, `SELECT key, value, secret_hint, updated_at, updated_by FROM settings ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type setting struct {
		key       string
		value     []byte
		hint      pgtype.Text
		updatedAt pgtype.Timestamptz
		updatedBy string
	}
	stored := make(map[string]setting)
	for rows.Next() {
		var row setting
		if err := rows.Scan(&row.key, &row.value, &row.hint, &row.updatedAt, &row.updatedBy); err != nil {
			return nil, err
		}
		stored[row.key] = row
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	specs := s.reg.All()
	out := make([]Entry, 0, len(specs))
	for _, spec := range specs {
		e := Entry{Spec: spec, Value: spec.Default}
		if spec.Kind == KindSecret {
			e.Value = nil
		}
		if row, ok := stored[spec.Key]; ok {
			e.Set = true
			if spec.Kind == KindSecret {
				e.Hint = row.hint.String
			} else {
				e.Value = row.value
			}
			e.UpdatedAt, e.UpdatedBy = row.updatedAt.Time, row.updatedBy
		}
		out = append(out, e)
	}
	return out, nil
}

// raw returns a key's stored bytes through the read-through cache, or nil and
// false when the key is not set. For a secret the bytes are the ciphertext —
// the cache never sees plaintext. A cached empty string is a known absence, a
// negative cache entry, so that keys sitting on their default do not hit the
// database on every read.
func (s *Store) raw(ctx context.Context, key string, secret bool) ([]byte, bool, error) {
	if s.cache != nil {
		if v, ok, err := s.cache.Get(ctx, cacheKey(key)); err == nil && ok {
			if len(v) == 0 {
				return nil, false, nil
			}
			return v, true, nil
		} else if err != nil {
			slog.WarnContext(ctx, "settings cache read failed, falling back to the database", "key", key, "error", err)
		}
	}
	column := "value"
	if secret {
		column = "secret_enc"
	}
	var v []byte
	err := s.db.QueryRow(ctx, `SELECT `+column+` FROM settings WHERE key = $1`, key).Scan(&v)
	if db.IsNoRows(err) || (err == nil && len(v) == 0) {
		s.cacheSet(ctx, key, nil)
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	s.cacheSet(ctx, key, v)
	return v, true, nil
}

func (s *Store) cacheSet(ctx context.Context, key string, v []byte) {
	if s.cache == nil {
		return
	}
	if err := s.cache.Set(ctx, cacheKey(key), v, cacheTTL); err != nil {
		slog.WarnContext(ctx, "settings cache write failed", "key", key, "error", err)
	}
}

// Invalidate drops this instance's copy and broadcasts the invalidation. The
// writer calls it after committing.
func (s *Store) Invalidate(ctx context.Context, key string) {
	if s.cache == nil {
		return
	}
	if err := s.cache.Delete(ctx, cacheKey(key)); err != nil {
		// A failed broadcast does not roll back a committed write; the TTL
		// bounds how stale the other instances can be.
		slog.ErrorContext(ctx, "settings invalidation broadcast failed; the change applies only once the TTL expires",
			"key", key, "error", err)
	}
}

// Get reads a key and unmarshals it into out; a key that is not set returns
// false. A secret is opened after the cache and unmarshals as a string.
//
// Other layers use it to read keys in their own namespace. This package supplies
// the mechanism and knows nothing about what those keys mean, which is why it
// offers a generic read rather than a typed accessor per key.
func (s *Store) Get(ctx context.Context, key string, out any) (bool, error) {
	raw, found, err := s.value(ctx, key)
	if err != nil || !found {
		return false, err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return false, err
	}
	return true, nil
}

// value is Get without the unmarshal: the effective stored JSON of a key, with
// secrets opened.
func (s *Store) value(ctx context.Context, key string) (json.RawMessage, bool, error) {
	spec, _ := s.reg.Lookup(key)
	raw, found, err := s.raw(ctx, key, spec.Kind == KindSecret)
	if err != nil || !found {
		return nil, false, err
	}
	if spec.Kind != KindSecret {
		return raw, true, nil
	}
	return s.open(key, raw)
}

func (s *Store) open(key string, sealed []byte) (json.RawMessage, bool, error) {
	if s.box == nil {
		return nil, false, ErrSecretUnreadable
	}
	plain, err := s.box.Open(sealed, []byte(key))
	if err != nil {
		return nil, false, fmt.Errorf("%w: %s", ErrSecretUnreadable, key)
	}
	return plain, true, nil
}

// RetentionSource is the one thing a cleanup or an archive job needs from the
// settings system: how many days a table keeps its rows.
//
// A narrow port rather than *Store on purpose — those jobs have no business
// knowing about the read-through cache or the invalidation broadcast. It is
// declared here, next to the only implementation, because that is upstream of
// every caller: two Cloud workers each declared this identical interface and
// the second one's comment already said so (ADR-0190).
type RetentionSource interface {
	RetentionDays(ctx context.Context, key string) int
}

// RetentionDays reads a retention key, falling back to that key's registered
// default when it is missing or unreadable.
//
// Falling back rather than failing: a sweeper that cannot read its setting
// should keep running on the default. Stopping means the table grows silently,
// which is worse than sweeping on a default window.
func (s *Store) RetentionDays(ctx context.Context, key string) int {
	var v int
	if found, err := s.Get(ctx, key, &v); err == nil && found && v > 0 {
		return v
	}
	return s.reg.defaultInt(key)
}

// Change is one key of a batch write. For a secret, Value `""` clears it.
type Change struct {
	Key   string
	Value json.RawMessage
}

// Apply writes a batch of changes on the caller's transaction, as one thing:
// every key is validated against its Spec, every section rule the batch
// touches is checked against the merged values, and only then is anything
// written. It returns the keys that were written, for the caller to invalidate
// after commit.
//
// Sections are locked with a transaction-scoped advisory lock. A row lock
// cannot serialize two first writers — the rows they would contend on do not
// exist yet — and two concurrent "configure this channel" writes that each pass
// the rule on its own merged view could leave the section inconsistent.
func (s *Store) Apply(ctx context.Context, tx pgx.Tx, changes []Change, updatedBy string) ([]string, error) {
	if err := s.plan(ctx, tx, changes, true); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(changes))
	for _, c := range changes {
		if err := s.set(ctx, tx, c.Key, c.Value, updatedBy); err != nil {
			return nil, err
		}
		keys = append(keys, c.Key)
	}
	return keys, nil
}

// Check evaluates a batch exactly as Apply would — Spec validation and section
// rules over the merged values — without writing or locking. An empty batch
// checks the stored state as it is, which is what the boot-time warning and
// `settings check` ask.
func (s *Store) Check(ctx context.Context, changes []Change) error {
	return s.plan(ctx, s.db, changes, false)
}

// EffectiveValues reads the effective values of the given sections outside any
// transaction: defaults overlaid with stored values, secrets opened. It is what
// a status page asks — "is this integration configured" is a question about the
// same merged view the rules judge.
func (s *Store) EffectiveValues(ctx context.Context, sections ...string) (Values, error) {
	return s.loadValues(ctx, s.db, sections)
}

// CheckSections runs every rule of every section against the stored values and
// returns the violations, one per failing rule, sorted by section. It is the
// read an operator or a boot log wants: not "is this write legal" but "what,
// as stored, is inconsistent".
func (s *Store) CheckSections(ctx context.Context) ([]*SectionError, error) {
	all := map[string]bool{}
	for _, section := range s.reg.Sections() {
		all[section] = true
	}
	rules, load := s.reg.rulesTouching(all)
	values, err := s.loadValues(ctx, s.db, load)
	if err != nil {
		return nil, err
	}
	var out []*SectionError
	for _, rule := range rules {
		if err := rule.Check(values); err != nil {
			out = append(out, &SectionError{Section: rule.Section, Err: err})
		}
	}
	return out, nil
}

// plan validates a batch against the registry and the section rules over the
// merged values, taking the section locks when asked; it writes nothing.
func (s *Store) plan(ctx context.Context, d dbtx, changes []Change, lock bool) error {
	touched := map[string]bool{}
	seen := map[string]bool{}
	for _, c := range changes {
		spec, ok := s.reg.Lookup(c.Key)
		if !ok {
			return fmt.Errorf("%w: %s", ErrUnknownKey, c.Key)
		}
		if seen[c.Key] {
			return &ValidationError{Key: c.Key, Err: errors.New("appears twice in one batch")}
		}
		seen[c.Key] = true
		if err := spec.Validate(c.Value); err != nil {
			return &ValidationError{Key: c.Key, Err: err}
		}
		if spec.Section != "" {
			touched[spec.Section] = true
		}
	}
	rules, load := s.reg.rulesTouching(touched)
	if len(rules) == 0 {
		return nil
	}
	if lock {
		for _, section := range load {
			if _, err := d.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "settings:"+section); err != nil {
				return fmt.Errorf("settings: lock section %s: %w", section, err)
			}
		}
	}
	values, err := s.loadValues(ctx, d, load)
	if err != nil {
		return err
	}
	for _, c := range changes {
		spec, _ := s.reg.Lookup(c.Key)
		if spec.Kind == KindSecret && secretClears(c.Value) {
			// The same reading set() gives a write: decoded, not the raw
			// bytes, so a non-canonical encoding cannot make the rule see
			// a value the write then deletes.
			delete(values, c.Key)
			continue
		}
		values[c.Key] = c.Value
	}
	for _, rule := range rules {
		if err := rule.Check(values); err != nil {
			return &SectionError{Section: rule.Section, Err: err}
		}
	}
	return nil
}

// secretClears reports whether a secret write is a clear: an empty string.
func secretClears(value json.RawMessage) bool {
	var plaintext string
	return json.Unmarshal(value, &plaintext) == nil && plaintext == ""
}

// loadValues reads the effective values of every key in the given sections:
// registered defaults, overlaid with what is stored. Secrets are opened in
// memory; an unreadable one counts as unset, so a rotated master key reads as
// "this channel needs its credentials again" rather than as a stuck row.
func (s *Store) loadValues(ctx context.Context, d dbtx, sections []string) (Values, error) {
	values := Values{}
	var keys []string
	for _, section := range sections {
		for _, spec := range s.reg.SectionKeys(section) {
			keys = append(keys, spec.Key)
			if len(spec.Default) > 0 && spec.Kind != KindSecret {
				values[spec.Key] = spec.Default
			}
		}
	}
	if len(keys) == 0 {
		return values, nil
	}
	sort.Strings(keys)
	rows, err := d.Query(ctx, `SELECT key, value, secret_enc FROM settings WHERE key = ANY($1)`, keys)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var value, sealed []byte
		if err := rows.Scan(&key, &value, &sealed); err != nil {
			return nil, err
		}
		if len(sealed) > 0 {
			plain, ok, err := s.open(key, sealed)
			if err != nil {
				slog.WarnContext(ctx, "stored secret is unreadable; treating it as unset", "key", key, "error", err)
			}
			if ok {
				values[key] = plain
			}
			continue
		}
		values[key] = value
	}
	return values, rows.Err()
}
