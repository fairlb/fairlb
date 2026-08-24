package gwstaffapi

import "testing"

// A nil catalog must stay a nil interface all the way down to the consumers.
//
// Asserted rather than reasoned about: the failure mode is a panic inside a
// `!= nil` guard, which reads as "the guard does not work" and sends the reader
// looking at the guard rather than at the conversion that produced the value.
func TestNilCatalogStaysNilInterface(t *testing.T) {
	if got := catalogInvalidatorOrNil(nil); got != nil {
		t.Fatal("a nil catalog produced a non-nil invalidator")
	}
}
