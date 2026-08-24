package money_test

import (
	"math"
	"testing"

	"github.com/fairlb/fairlb/foundation/money"
)

func TestFormatNano(t *testing.T) {
	cases := []struct {
		nano int64
		want string
	}{
		{5_000_000_000_000, "5000.00"},
		{1_230_000_000, "1.23"},
		{9_000_000, "0.00"}, // below 0.01, truncated
		{10_000_000, "0.01"},
		{-2_500_000_000, "-2.50"},
		{0, "0.00"},
		{math.MaxInt64, "9223372036.85"},
		{math.MinInt64, "-9223372036.85"}, // guard: negating must not overflow
	}
	for _, c := range cases {
		if got := money.FormatNano(c.nano); got != c.want {
			t.Errorf("FormatNano(%d) = %q, want %q", c.nano, got, c.want)
		}
	}
}

// The wire form's sign, pinned by value.
//
// Three hand-written copies of this formatter stood in the tree and two were
// wrong here, each in its own way, and nothing reported either — every call
// site happened to guard its input, so the defect waited for the first one that
// did not. The worse of the two answered a negative amount with a *positive*
// number.
//
// The cases below the major unit are the ones that mattered: both copies were
// right for -1.5 and wrong for -0.5, because they let the sign live in the
// whole part, which only carries it while the absolute value is at least one.
func TestFormatNanoExactCarriesTheSign(t *testing.T) {
	for _, tc := range []struct {
		nano int64
		want string
	}{
		{0, "0"},
		{1, "0.000000001"},
		{100_000_000, "0.1"},
		{999_999_999, "0.999999999"},
		{1_000_000_000, "1"},
		{1_500_000_000, "1.5"},
		{1_000_000_001, "1.000000001"},
		// Below one, where the whole part is zero and cannot carry a sign.
		{-1, "-0.000000001"},
		{-500_000_000, "-0.5"},
		{-999_999_999, "-0.999999999"},
		// At and above one.
		{-1_000_000_000, "-1"},
		{-1_500_000_000, "-1.5"},
		{math.MinInt64, "-9223372036.854775808"},
		{math.MaxInt64, "9223372036.854775807"},
	} {
		if got := money.FormatNanoExact(tc.nano); got != tc.want {
			t.Errorf("money.FormatNanoExact(%d) = %q, want %q", tc.nano, got, tc.want)
		}
	}
}

// The display form is a different function on purpose: two decimals, truncated.
// Reaching both through one name would give a caller who wants "$1.50" and a
// caller who has to transmit 0.000000001 the same answer, and only one of them
// would be right.
func TestFormatNanoAndFormatNanoExactAreNotInterchangeable(t *testing.T) {
	if money.FormatNano(1) == money.FormatNanoExact(1) {
		t.Error("the display form must not render 1 nano the way the wire form does")
	}
	if money.FormatNano(1_500_000_000) == money.FormatNanoExact(1_500_000_000) {
		t.Error(`display renders "1.50" and the wire form "1.5"; they are not the same string`)
	}
}

// 两个判定的接受面差在哪，逐条钉住。
//
// 它们此前是三份散在三个包里的正则，且已经漂了。合并的风险不是「合错了」，是
// 「合的时候把某一处的接受面悄悄改宽或改窄」——改宽让一个不该进库的字面量进了
// 账单，改窄让一个一直能用的设置值突然被拒。所以判据按**差别**写，不按「能过」写。
func TestDecimalAcceptSets(t *testing.T) {
	both := []string{"0", "1", "1.5", "0.000000001", "123456789.123"}
	for _, s := range both {
		if !money.IsSignedDecimal(s) {
			t.Errorf("IsSignedDecimal(%q) = false，两种接受面都该收下它", s)
		}
		if !money.IsPlainDecimal(s, 0) {
			t.Errorf("IsPlainDecimal(%q, 0) = false，两种接受面都该收下它", s)
		}
	}

	// 差别一：符号。设置值不知道那个数是什么意思，故收；价格进账单，故拒。
	for _, s := range []string{"-1", "-0.5"} {
		if !money.IsSignedDecimal(s) {
			t.Errorf("IsSignedDecimal(%q) = false，通用设置值该收负数", s)
		}
		if money.IsPlainDecimal(s, 0) {
			t.Errorf("IsPlainDecimal(%q, 0) = true，价格不该收负数", s)
		}
	}

	// 差别二：前导零。同上——`007` 作为设置值无害，作为费率是有人打错了。
	if !money.IsSignedDecimal("007") {
		t.Error(`IsSignedDecimal("007") = false，通用设置值不该管前导零`)
	}
	if money.IsPlainDecimal("007", 0) {
		t.Error(`IsPlainDecimal("007", 0) = true，费率不该收前导零`)
	}

	// 差别三：小数位上限。places 只对 IsPlainDecimal 有意义，且必须真的起作用——
	// 若 0 与 9 给出同样的答案，这个参数就是死的。
	const tenPlaces = "0.0000000001"
	if !money.IsPlainDecimal(tenPlaces, 0) {
		t.Errorf("IsPlainDecimal(%q, 0) = false，不限位数时应当收下", tenPlaces)
	}
	if money.IsPlainDecimal(tenPlaces, 9) {
		t.Errorf("IsPlainDecimal(%q, 9) = true，十位小数超出九位上限", tenPlaces)
	}
	if !money.IsPlainDecimal("0.123456789", 9) {
		t.Error(`IsPlainDecimal("0.123456789", 9) = false，恰好九位应当收下`)
	}

	// 两者共同拒绝的：科学计数法与两侧空白。一个只通过没人复核过的格式到达网关的
	// 费率，正是那种最后差三个数量级的数。
	for _, s := range []string{"1e9", "1E9", " 1", "1 ", "", ".5", "1.", "abc"} {
		if money.IsSignedDecimal(s) {
			t.Errorf("IsSignedDecimal(%q) = true", s)
		}
		if money.IsPlainDecimal(s, 0) {
			t.Errorf("IsPlainDecimal(%q, 0) = true", s)
		}
	}
}
