package proxy

import (
	"strings"
	"testing"

	"github.com/fairlb/fairlb/foundation/errcode"
)

// Write falls back to gateway.internal for any code it has no shape for. That
// is the right behaviour for an unknown string and the wrong one for a code
// this build can actually emit: the caller gets a 500 and a generic message
// where a 400 and a specific one were intended, and nothing anywhere says so.
//
// Measured while adding the video plane: two freshly registered codes rendered
// as `gateway.internal` with "temporarily unavailable", and only an end-to-end
// test caught it. This is the gate that would have caught it first.
func TestEveryGatewayCodeHasARenderableShape(t *testing.T) {
	for code := range errcode.All() {
		if !strings.HasPrefix(code, "gateway.") {
			continue // other layers render through problem+json, not through Write
		}
		if _, ok := shapes[code]; !ok {
			t.Errorf("error code %q has no entry in shapes, so Write renders it as "+
				"gateway.internal with a generic 500 message. Add its dialect types "+
				"beside the others.", code)
		}
	}
}

// The reverse direction: a shape for a code no build can emit is a dead row
// that outlives whatever removed the code.
func TestNoShapeSurvivesItsErrorCode(t *testing.T) {
	all := errcode.All()
	for code := range shapes {
		if _, ok := all[code]; !ok {
			t.Errorf("shapes has an entry for %q, which is not a registered error code", code)
		}
	}
}
