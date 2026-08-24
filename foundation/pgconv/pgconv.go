// Package pgconv holds the small, repeatedly needed conversions between database
// types and API responses.
//
// # Why this is worth its own package
//
// Each function is three or four lines and copying one is easy. The cost shows
// up when one of them changes. "An unencodable value becomes nothing" and "NULL
// maps to an absent field rather than the zero time" are judgements, and a
// judgement duplicated N times drifts into N different judgements. This
// repository once carried four separate timestamp conversions, one of which had
// an extra normalization step. Nobody made them different on purpose.
package pgconv

import (
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// TimePtr maps a nullable timestamp to a pointer: NULL becomes nil, so the JSON
// field is absent entirely rather than rendered as the zero time — which a client
// would read as a real instant.
//
// It deliberately does not normalize to UTC; the location the driver returned is
// passed through. A caller that wants normalization adds it. Whether to
// normalize is a presentation choice belonging to that API, not something a
// generic conversion should decide on its behalf.
func TimePtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

// JSONOrNil encodes a value whose shape the spec already constrains into a jsonb
// parameter, returning nil if it cannot be encoded.
//
// Swallowing the error is the right handling for this input domain rather than
// laziness: the input comes from generated, strongly typed structs, and none of
// the shapes that fail to encode (channels, functions, cycles) can occur. If one
// ever does, the generator changed its behavior, and the right response is to
// change this function — not to turn a create request into a 500.
//
// It is not called mustJSON: it does not panic, and in Go the must prefix means
// exactly that.
func JSONOrNil(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return raw
}

// Text is the pgtype form of an optional string: empty means "not supplied",
// which is what a nullable text column and a LIKE filter both want.
func Text(s string) pgtype.Text {
	return pgtype.Text{String: s, Valid: s != ""}
}
