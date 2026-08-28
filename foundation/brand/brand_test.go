package brand_test

import (
	"testing"

	"github.com/fairlb/fairlb/foundation/brand"
)

// The default matches the default profile, which is what a deployment with no
// bundle mounted serves.
func TestDefaultNameIsTheDefaultProfile(t *testing.T) {
	if brand.Name != "FairLB" {
		t.Fatalf("brand.Name = %q", brand.Name)
	}
}
