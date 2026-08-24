package gwstaffapi

import "testing"

// Every outcome a run can report has a place on the wire.
//
// This is an internal test because the thing worth checking is internal: the
// map between two enums that live in two documents. The failure it guards
// against is not a rename -- a rename breaks the map literal at compile time --
// but an addition. A seventh outcome added to the run and not to the map turns
// into an empty string in the response, which every reader downstream treats as
// an unknown state, and nothing anywhere says why.
func TestEveryImportOutcomeHasAWireValue(t *testing.T) {
	all := []ImportOutcome{
		ImportPriced, ImportUpdated, ImportUnchanged,
		ImportKept, ImportVerified, ImportSkipped,
	}
	// The list above is written by hand, so it is worth knowing it is not the
	// one that went stale: it and the map have to cover each other exactly.
	if len(all) != len(importOutcomeOut) {
		t.Fatalf("this test knows %d outcomes and the wire mapping has %d; "+
			"one of the two was not updated", len(all), len(importOutcomeOut))
	}
	for _, o := range all {
		wire, ok := importOutcomeOut[o]
		if !ok {
			t.Errorf("outcome %q has no wire value, so the response would carry an empty one", o)
			continue
		}
		if !wire.Valid() {
			t.Errorf("outcome %q maps to %q, which the contract does not declare", o, wire)
		}
	}
}
