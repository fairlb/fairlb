package proxy

import (
	"testing"

	"github.com/fairlb/fairlb/foundation/config"
)

// The default shutdown window must cover the longest built-in request budget.
func TestDefaultShutdownBudgetCoversImageRequests(t *testing.T) {
	if config.DefaultShutdownTimeout <= imageTimeout {
		t.Fatalf("default shutdown timeout %v must exceed image timeout %v", config.DefaultShutdownTimeout, imageTimeout)
	}
}
