package brand_test

import (
	"testing"

	"github.com/fairlb/fairlb/foundation/brand"
)

// The default matches the default profile. A white-label build overrides it at
// link time; nothing at runtime may assign it.
func TestDefaultNameIsTheDefaultProfile(t *testing.T) {
	if brand.Name != "FairLB" {
		t.Fatalf("brand.Name = %q", brand.Name)
	}
}
