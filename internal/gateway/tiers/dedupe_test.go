package tiers

import "testing"

import "github.com/google/uuid"

// Duplicate ids would be harmless — the insert carries ON CONFLICT DO NOTHING —
// so what this protects is the *response*: the caller gets back the set it
// asked for, in the order it asked, rather than one with its own duplicates
// echoed at it.
func TestDedupeKeepsFirstOccurrenceOrder(t *testing.T) {
	a := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	b := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	c := uuid.MustParse("33333333-3333-3333-3333-333333333333")

	got := dedupe([]uuid.UUID{b, a, b, c, a})
	want := []uuid.UUID{b, a, c}
	if len(got) != len(want) {
		t.Fatalf("dedupe returned %d ids, want %d", len(got), len(want))
	}
	for i := range want {
		if uuid.UUID(got[i].Bytes) != want[i] || !got[i].Valid {
			t.Fatalf("dedupe[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

// An empty set is how "this tier admits nothing by list" is expressed, and it
// has to survive as an empty slice rather than becoming nil: the caller writes
// it straight into the insert's argument.
func TestDedupeEmptyStaysNonNil(t *testing.T) {
	got := dedupe(nil)
	if got == nil {
		t.Fatal("dedupe(nil) returned a nil slice")
	}
	if len(got) != 0 {
		t.Fatalf("dedupe(nil) returned %d ids", len(got))
	}
}
