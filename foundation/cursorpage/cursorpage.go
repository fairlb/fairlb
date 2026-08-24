// Package cursorpage is the single place cursor pagination is encoded, clamped
// and probed for a last page.
//
// Five paginated endpoints once carried four different cursor encodings and no
// shared implementation. There is exactly one form here:
// base64url("<created_at in microseconds>:<uuid>"), anchored on the composite
// (created_at, id) key — most tables are indexed on (org_id, created_at DESC),
// and only a composite key uses that index.
//
// The sort key has to be immutable. Paginating on a column that changes (a
// last-seen timestamp, say) breaks the premise of keyset pagination: rows move
// under the reader, so pages skip and repeat.
package cursorpage

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// ErrInvalid is the sentinel for every decode failure; callers should match on
// it and render a 400. Passing a bare error through instead renders a 500, so
// the same malformed cursor answers differently on different endpoints.
var ErrInvalid = errors.New("cursorpage: invalid cursor")

type Page struct {
	CursorAt pgtype.Timestamptz
	CursorID pgtype.UUID
	Limit    int32
}

func (p Page) ProbeLimit() int32 { return p.Limit + 1 }

// Parse converts optional HTTP query values into database-ready pagination
// parameters. Presentation layers may map ErrInvalid to their validation
// response without reimplementing cursor and UUID parsing.
func Parse(cursor *string, limit *int, def, max int) (Page, error) {
	requested := 0
	if limit != nil {
		requested = *limit
	}
	page := Page{Limit: int32(Clamp(requested, def, max))}
	if cursor == nil || *cursor == "" {
		return page, nil
	}
	at, rawID, err := Decode(*cursor)
	if err != nil || page.CursorID.Scan(rawID) != nil {
		return Page{}, ErrInvalid
	}
	page.CursorAt = pgtype.Timestamptz{Time: at, Valid: true}
	return page, nil
}

// Encode produces an opaque cursor. id is the uuid in text form, with or without
// hyphens; Decode returns it exactly as given.
func Encode(createdAt time.Time, id string) string {
	raw := fmt.Sprintf("%d:%s", createdAt.UnixMicro(), id)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// Decode unpacks a cursor. Every malformed shape — bad base64, missing
// separator, non-numeric timestamp, empty id — is ErrInvalid: as far as the
// client is concerned there is one fact, "that cursor is not valid", and the
// internals do not leak.
func Decode(s string) (createdAt time.Time, id string, err error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	micros, rest, ok := strings.Cut(string(raw), ":")
	if !ok || rest == "" {
		return time.Time{}, "", ErrInvalid
	}
	us, err := strconv.ParseInt(micros, 10, 64)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return time.UnixMicro(us).UTC(), rest, nil
}

// Clamp constrains a requested limit to [1, max]; zero or negative — including
// an absent parameter — yields def.
func Clamp(limit, def, max int) int {
	if limit <= 0 {
		return def
	}
	if limit > max {
		return max
	}
	return limit
}

// Trim implements the limit+1 probe: the query fetches one extra row, and this
// trims back to the page and answers whether another page exists. Testing
// len == limit instead hands out a spurious empty page whenever the total
// divides evenly by the limit.
func Trim[T any](rows []T, limit int) (page []T, more bool) {
	if len(rows) > limit {
		return rows[:limit], true
	}
	return rows, false
}

// ===== Ordered keys that are not time =====
//
// Most of the paginated surfaces here are feeds, ordered newest-first on
// (created_at, id). Configuration lists are not feeds: credentials read best
// grouped by platform, the model catalogue by slug, routes by priority. Those
// orders are the whole point of the screen, and paginating them on created_at
// instead would reorder the page to suit the cursor.
//
// So the key varies while the envelope does not. Everything above the key —
// opaque to the client, one ErrInvalid for every malformed shape, Clamp,
// ProbeLimit, Trim — is shared; only what gets packed differs.

// KeyPage is the pagination parameters for a list ordered by a text key.
type KeyPage struct {
	// Key is the last row of the previous page, one element per ORDER BY
	// column. Empty means the first page.
	Key   []string
	Limit int32
}

func (p KeyPage) ProbeLimit() int32 { return p.Limit + 1 }

// At returns the i-th key component, or "" past the end. sqlc parameters are
// positional and a first-page request has no key at all, so the query needs a
// value for every component either way; the SQL distinguishes the two cases on
// the cursor being absent, not on the component being empty.
func (p KeyPage) At(i int) string {
	if i < len(p.Key) {
		return p.Key[i]
	}
	return ""
}

// HasKey reports whether this is a continuation rather than a first page.
func (p KeyPage) HasKey() bool { return len(p.Key) > 0 }

// BoolAt reads a boolean key component written by EncodeKey as "t" or "f".
//
// Anything else is ErrInvalid rather than false: arity alone cannot tell a
// cursor minted by another two-component endpoint from this one, and reading
// a vendor name as "not the default" would seek into the wrong order — a page
// that looks plausible and is wrong. A first-page request (no key) reads false.
func (p KeyPage) BoolAt(i int) (bool, error) {
	if !p.HasKey() {
		return false, nil
	}
	switch p.At(i) {
	case "t":
		return true, nil
	case "f":
		return false, nil
	default:
		return false, ErrInvalid
	}
}

// EncodeKey packs an ordered key into an opaque cursor.
//
// Components are joined with NUL, and that separator is safe for a reason
// rather than by luck: a PostgreSQL text value cannot contain U+0000 at all, so
// no component can ever carry one. Any printable separator would eventually be
// eaten by a credential named "eu:prod" or a slug with a dash convention nobody
// anticipated.
func EncodeKey(parts ...string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strings.Join(parts, "\x00")))
}

// DecodeKey unpacks a cursor produced by EncodeKey.
//
// A cursor with the wrong number of components is ErrInvalid, not a short
// slice. Wrong arity means it was minted by a different endpoint, and reading
// it as if it belonged here would seek into the wrong order — which produces a
// page that looks plausible and is wrong, the worst available outcome.
func DecodeKey(s string, want int) ([]string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	parts := strings.Split(string(raw), "\x00")
	if len(parts) != want {
		return nil, ErrInvalid
	}
	return parts, nil
}

// ParseKeyPage is Parse's sibling for text-keyed lists.
func ParseKeyPage(cursor *string, limit *int, want, def, max int) (KeyPage, error) {
	requested := 0
	if limit != nil {
		requested = *limit
	}
	page := KeyPage{Limit: int32(Clamp(requested, def, max))}
	if cursor == nil || *cursor == "" {
		return page, nil
	}
	parts, err := DecodeKey(*cursor, want)
	if err != nil {
		return KeyPage{}, ErrInvalid
	}
	page.Key = parts
	return page, nil
}
