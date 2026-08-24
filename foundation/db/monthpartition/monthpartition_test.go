package monthpartition_test

import (
	"testing"
	"time"

	"github.com/fairlb/fairlb/foundation/db/monthpartition"
)

func TestBoundsUTC(t *testing.T) {
	start, end := monthpartition.BoundsUTC(time.Date(2026, time.March, 31, 23, 0, 0, 0, time.FixedZone("west", -7*60*60)))
	if want := time.Date(2026, time.April, 1, 0, 0, 0, 0, time.UTC); !start.Equal(want) {
		t.Fatalf("start = %s, want %s", start, want)
	}
	if want := time.Date(2026, time.May, 1, 0, 0, 0, 0, time.UTC); !end.Equal(want) {
		t.Fatalf("end = %s, want %s", end, want)
	}
}
