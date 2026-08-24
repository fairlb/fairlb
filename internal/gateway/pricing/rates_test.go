package pricing

import (
	"math"
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

// The map is the only thing standing between the column's six values and a
// price that cannot be described. A missing entry makes ModelPricing refuse the
// whole model, so the map being total over the CHECK is a real invariant.
func TestBucketMapCoversTheColumn(t *testing.T) {
	// Kept as a literal, not derived from bucketFromDB: a test that reads its
	// expectation out of the thing under test asserts nothing.
	columnValues := []string{"in", "out", "cache_read", "cache_write", "audio_in", "audio_out"}
	if len(bucketFromDB) != len(columnValues) {
		t.Fatalf("map has %d entries, the column allows %d", len(bucketFromDB), len(columnValues))
	}
	for _, v := range columnValues {
		bucket, ok := bucketFromDB[v]
		if !ok {
			t.Errorf("column value %q has no bucket", v)
			continue
		}
		if string(bucket) != v {
			t.Errorf("bucket for %q is %q -- the two vocabularies must stay identical", v, bucket)
		}
	}
}
