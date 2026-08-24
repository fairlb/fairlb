package pgconv_test

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/foundation/pgconv"
)

// NULL must map to nil rather than the zero time, which renders in JSON as
// 0001-01-01T00:00:00Z and reads to a client as a real instant.
func TestTimePtrNullBecomesNil(t *testing.T) {
	if got := pgconv.TimePtr(pgtype.Timestamptz{}); got != nil {
		t.Fatalf("NULL should map to nil, got %v", got)
	}
	want := time.Date(2026, 8, 14, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	got := pgconv.TimePtr(pgtype.Timestamptz{Time: want, Valid: true})
	if got == nil || !got.Equal(want) {
		t.Fatalf("a present value should pass through unchanged, got %v", got)
	}
	// No UTC normalization; whether to normalize is each API's own presentation
	// choice (see the package comment).
	if got.Location() != want.Location() {
		t.Errorf("the location must not be rewritten: want %v, got %v", want.Location(), got.Location())
	}
}

func TestJSONOrNil(t *testing.T) {
	if got := string(pgconv.JSONOrNil(map[string]int{"tpm": 60})); got != `{"tpm":60}` {
		t.Fatalf("an ordinary value should encode, got %q", got)
	}
	// An unencodable value returns nil, not a panic and not the literal "null":
	// the latter would store a JSON null in the column, which is not the same
	// thing as the field being unset.
	if got := pgconv.JSONOrNil(make(chan int)); got != nil {
		t.Fatalf("an unencodable value should return nil, got %q", got)
	}
}
