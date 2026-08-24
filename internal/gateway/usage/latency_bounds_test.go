package usage

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"testing"
)

// The rendered seconds must be the stored milliseconds, and nothing else.
//
// The risk this guards is a future reader deciding the conversion loop is
// needless indirection and writing the seconds out as a literal list. That
// version is correct on the day it is written and silently wrong the first time
// a bound is tuned, because only one of the two lists gets edited -- and the
// symptom is two percentile readings that disagree with no indication which one
// moved.
func TestLatencyBoundsSecondsIsDerivedFromTheStoredBounds(t *testing.T) {
	got := LatencyBoundsSeconds()
	if len(got) != len(latencyBounds) {
		t.Fatalf("bound count: got %d, want %d", len(got), len(latencyBounds))
	}
	for i, ms := range latencyBounds {
		if want := float64(ms) / 1000; got[i] != want {
			t.Errorf("bound %d: got %v, want %v", i, got[i], want)
		}
	}
}

// The bucket bounds and the rollup's lat_le_* columns are one-to-one, which is
// what the comment on latencyBounds asserts and what makes a percentile read
// from Prometheus comparable with one read from the rollups.
//
// Read out of the migration rather than out of a second hand-written list: a
// list would be one more thing to keep in step, which is the failure this test
// exists to catch.
func TestLatencyBoundsMatchTheRollupColumns(t *testing.T) {
	schema, err := os.ReadFile("../../../migrations/0002_gateway.sql")
	if err != nil {
		t.Fatalf("read the gateway migration: %v", err)
	}
	found := regexp.MustCompile(`(?m)^\s*lat_le_(\d+)\s`).FindAllStringSubmatch(string(schema), -1)
	if len(found) == 0 {
		// A zero reading here would let the test pass while measuring nothing.
		t.Fatal("no lat_le_* columns found in the migration: this test is pointed at nothing")
	}
	var cols []int64
	for _, m := range found {
		n, convErr := strconv.ParseInt(m[1], 10, 64)
		if convErr != nil {
			t.Fatalf("column suffix %q is not a number: %v", m[1], convErr)
		}
		cols = append(cols, n)
	}
	if fmt.Sprint(cols) != fmt.Sprint(latencyBounds) {
		t.Errorf("rollup columns %v do not match latencyBounds %v", cols, latencyBounds)
	}
}
