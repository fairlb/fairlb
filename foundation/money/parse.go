package money

import (
	"errors"
	"fmt"
	"strings"
)

// ErrNotDecimal is returned by ParseDecimalNano for anything that is not a
// plain decimal amount.
var ErrNotDecimal = errors.New("money: not a decimal amount")

// ParseDecimalNano parses a display-format decimal string ("29.00", "1000",
// "0.000000001") into nano units without ever touching a float.
//
// Payment providers put amounts on the wire as decimal strings; the two ways
// to get them wrong are to parse with strconv.ParseFloat (29.99 becomes
// 29.989999…, and the reconciliation against the order then fails by one nano)
// or to accept more fractional digits than nano can hold and round silently.
// This accepts at most nine fractional digits and rejects the tenth instead of
// rounding: an amount that does not fit is a contract violation, not a value.
//
// Grammar: optional leading '-', one or more ASCII digits, optionally followed
// by '.' and one to nine digits. No whitespace, no exponent, no thousands
// separators, no currency symbol, no leading '+'.
func ParseDecimalNano(s string) (int64, error) {
	raw := s
	negative := strings.HasPrefix(s, "-")
	if negative {
		s = s[1:]
	}
	whole, fraction, hasDot := strings.Cut(s, ".")
	if whole == "" || !allDigits(whole) {
		return 0, fmt.Errorf("%w: %q", ErrNotDecimal, raw)
	}
	if hasDot && (fraction == "" || len(fraction) > 9 || !allDigits(fraction)) {
		return 0, fmt.Errorf("%w: %q", ErrNotDecimal, raw)
	}
	var n int64
	for _, c := range whole {
		d := int64(c - '0')
		if n > (1<<63-1-d)/10 {
			return 0, fmt.Errorf("%w: %q overflows", ErrNotDecimal, raw)
		}
		n = n*10 + d
	}
	if n > (1<<63-1)/nanoPerUnit {
		return 0, fmt.Errorf("%w: %q overflows", ErrNotDecimal, raw)
	}
	n *= nanoPerUnit
	if hasDot {
		scale := int64(nanoPerUnit)
		var frac int64
		for _, c := range fraction {
			scale /= 10
			frac += int64(c-'0') * scale
		}
		if n > (1<<63-1)-frac {
			return 0, fmt.Errorf("%w: %q overflows", ErrNotDecimal, raw)
		}
		n += frac
	}
	if negative {
		n = -n
	}
	return n, nil
}

func allDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
