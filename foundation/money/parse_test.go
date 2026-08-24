package money_test

import (
	"errors"
	"math"
	"testing"

	"github.com/fairlb/fairlb/foundation/money"
)

func TestParseDecimalNano(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"29.00", 29_000_000_000},
		{"29.99", 29_990_000_000}, // the float trap: ParseFloat gives 29.989999…
		{"1000", 1_000_000_000_000},
		{"0", 0},
		{"0.000000001", 1},
		{"0.5", 500_000_000},
		{"-10.25", -10_250_000_000},
		{"9223372036.854775807", math.MaxInt64},
	}
	for _, c := range cases {
		got, err := money.ParseDecimalNano(c.in)
		if err != nil || got != c.want {
			t.Errorf("ParseDecimalNano(%q) = %d, %v; want %d", c.in, got, err, c.want)
		}
	}
	// Round trip against the wire formatter: what FormatNanoExact emits must parse back.
	for _, nano := range []int64{0, 1, -1, 1_500_000_000, math.MaxInt64, math.MinInt64 + 1} {
		got, err := money.ParseDecimalNano(money.FormatNanoExact(nano))
		if err != nil || got != nano {
			t.Errorf("round trip %d via %q = %d, %v", nano, money.FormatNanoExact(nano), got, err)
		}
	}
}

func TestParseDecimalNanoRejects(t *testing.T) {
	for _, in := range []string{
		"", "-", ".", "1.", ".5", "+1", " 1", "1 ", "1,000", "$1", "1e3", "abc", "1.2.3",
		"0.0000000001",         // ten fractional digits: would have to round
		"9223372036.854775808", // MaxInt64 + 1 nano
		"9223372037",           // whole part alone overflows
		"99999999999999999999", // overflows before scaling
	} {
		if got, err := money.ParseDecimalNano(in); !errors.Is(err, money.ErrNotDecimal) {
			t.Errorf("ParseDecimalNano(%q) = %d, %v; want ErrNotDecimal", in, got, err)
		}
	}
}
