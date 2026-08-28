package pricing

import (
	"math"
	"os"
	"regexp"
	"slices"
	"testing"

	"github.com/google/uuid"
)

// The markup rounds **up**, and the arithmetic has to survive rates the column
// still allows.
//
// These two properties used to live in the staff handler package, where the
// only thing exercising them was an end-to-end test that also had to stand up a
// database. Moving the rule into the domain is what makes it testable as a rule.
func TestCeilRate(t *testing.T) {
	for _, tc := range []struct {
		name       string
		nano       int64
		multiplier int32
		want       int64
	}{
		{"cost is the identity", 1_500_000, 10000, 1_500_000},
		{"a markup rounds up, never down", 1, 10001, 2},
		{"an exact result is not nudged", 100, 15000, 150},
		{"zero stays zero at any markup", 0, 100000, 0},
		{"a discount still rounds up", 3, 5000, 2},
		{
			// The two column CHECKs are chosen so this pair is the largest that
			// can occur, and it still fits. That is the whole reason the
			// overflow branch below is unreachable in production.
			"the largest allowed rate at the largest allowed markup",
			92233720368547758, 100000, 922337203685477580,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ceilRate(tc.nano, tc.multiplier)
			if !ok {
				t.Fatalf("ceilRate(%d, %d) reported overflow", tc.nano, tc.multiplier)
			}
			if got != tc.want {
				t.Fatalf("ceilRate(%d, %d) = %d, want %d", tc.nano, tc.multiplier, got, tc.want)
			}
		})
	}
}

// A result past int64 must be refused, not truncated.
//
// The input cannot occur while both column CHECKs hold — which is exactly why
// this is worth asserting: the previous implementation ended in a bare
// `n.Int64()`, so the day either CHECK is relaxed the failure is a negative
// price with nothing to say it happened.
func TestCeilRateRefusesRatherThanWrapping(t *testing.T) {
	if got, ok := ceilRate(math.MaxInt64/2, 30000); ok {
		t.Fatalf("expected a refusal, got %d", got)
	}
}

func TestPublishedRefusesOnOverflow(t *testing.T) {
	_, err := published(TokenRates{Input: math.MaxInt64 / 2}, 30000, uuid.UUID{})
	if err == nil {
		t.Fatal("expected published to refuse an unrepresentable rate")
	}
}

// KnownBuckets is the single list, and this is what makes it single: it has to
// be exactly what the column accepts.
//
// The expectation is read out of the migration rather than written here as a
// literal. A literal is what this test used to hold, and it went stale the day
// `image_in` was added to the column: the test kept passing on six values while
// the column allowed seven, and every image-input rate the reference import
// produced was refused as an unknown bucket with nothing to say so. Reading the
// migration is not "taking the expectation from the thing under test" -- the
// migration *is* the column, and the Go list is what is under test.
//
// The two round-trip maps are built from KnownBuckets, so this one assertion
// covers all three. That they really are built from it, rather than written out
// beside it, is asserted below.
func TestKnownBucketsMatchTheColumn(t *testing.T) {
	columnValues := bucketCheckValues(t)
	known := KnownBuckets()
	if len(known) != len(columnValues) {
		t.Fatalf("KnownBuckets has %d entries, the column allows %d (%v)",
			len(known), len(columnValues), columnValues)
	}
	for _, v := range columnValues {
		if !slices.Contains(known, Bucket(v)) {
			t.Errorf("column value %q is not in KnownBuckets, so nothing can write it", v)
		}
	}
}

// The maps are derived, not restated. Asserted rather than assumed, because
// "derived" is a property of two lines of code that a later edit can quietly
// turn back into a literal -- which is the shape the drift above started from.
func TestBucketMapsAreTotalOverKnownBuckets(t *testing.T) {
	known := KnownBuckets()
	if len(bucketToDB) != len(known) || len(bucketFromDB) != len(known) {
		t.Fatalf("maps have %d and %d entries against %d known buckets",
			len(bucketToDB), len(bucketFromDB), len(known))
	}
	for _, b := range known {
		stored, ok := bucketToDB[b]
		if !ok {
			t.Errorf("%q has no stored name, so saving that rate is refused as unknown", b)
			continue
		}
		if stored != string(b) {
			t.Errorf("stored name for %q is %q -- the two vocabularies must stay identical", b, stored)
		}
		if back, ok := bucketFromDB[stored]; !ok || back != b {
			t.Errorf("%q does not round-trip: read back as %q (present=%v)", b, back, ok)
		}
	}
}

// bucketCheckValues reads the accepted bucket names out of the migration's
// CHECK constraint. Failing to find it is a test failure rather than an empty
// list: an empty expectation would make the assertion above vacuous, which is
// the one outcome worse than a wrong one.
func bucketCheckValues(t *testing.T) []string {
	t.Helper()
	const migration = "../../../migrations/0002_gateway.sql"
	sql, err := os.ReadFile(migration)
	if err != nil {
		t.Fatalf("read %s: %v", migration, err)
	}
	// The constraint spans lines, so the region is located first and the
	// literals are pulled out of it.
	re := regexp.MustCompile(`(?s)bucket\s+text NOT NULL CHECK \(\s*bucket IN \((.*?)\)`)
	m := re.FindSubmatch(sql)
	if m == nil {
		t.Fatalf("no bucket CHECK found in %s; this test cannot assert anything", migration)
	}
	values := regexp.MustCompile(`'([a-z_]+)'`).FindAllStringSubmatch(string(m[1]), -1)
	if len(values) == 0 {
		t.Fatalf("bucket CHECK in %s lists no values", migration)
	}
	out := make([]string, 0, len(values))
	for _, v := range values {
		out = append(out, v[1])
	}
	return out
}
