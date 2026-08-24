// Package money holds the helpers for displaying amounts. Amounts are always an
// int64 count of nano units plus a currency; storage and arithmetic stay in nano
// units, and only display converts to the major unit.
package money

import (
	"fmt"
	"math/big"
	"regexp"
	"strings"
)

// NanoPerUnit is how many nano units make one major unit. Exported because the
// literal was written out at five call sites, each one a place where a typo in
// the number of zeros would be arithmetically silent.
const NanoPerUnit = 1_000_000_000

// nanoPerUnit keeps the unexported spelling for this file's own arithmetic.
const nanoPerUnit = NanoPerUnit

// FormatNano renders nano units as a major-unit string with two decimals. It
// carries no currency symbol; the caller adds one. The value is truncated rather
// than rounded, and the sign is preserved. The absolute value is taken in uint64
// so that the most negative int64 does not overflow when negated.
func FormatNano(nano int64) string {
	neg := nano < 0
	var abs uint64
	if neg {
		abs = uint64(-(nano + 1)) + 1 // absolute value that is safe for MinInt64
	} else {
		abs = uint64(nano)
	}
	major := abs / nanoPerUnit
	cents := (abs % nanoPerUnit) / (nanoPerUnit / 100)
	sign := ""
	if neg {
		sign = "-"
	}
	return fmt.Sprintf("%s%d.%02d", sign, major, cents)
}

// FormatNanoExact renders nano units at full precision, dropping meaningless
// trailing zeros: 1_500_000_000 is "1.5", 1_000_000_000 is "1", 1 is
// "0.000000001".
//
// This is the wire form. Money crosses the wire as a string, never as a JSON
// number: a number goes through the client's float parser and then no two
// clients agree about the low decimal places.
//
// It is deliberately not FormatNano. That one is the *display* form — two
// decimals, truncated — and the two must not be reachable through one name:
// a caller who wants to show "$1.50" and a caller who has to transmit
// 0.000000001 want different, both-correct answers.
//
// # The sign
//
// Three hand-written copies of this stood in the tree, and two of them were
// wrong for negatives in a way nothing reported, because every call site
// happened to guard its input:
//
//	catalog: -500_000_000 -> "0.-5"   (and -1 -> "0.0000000-1")
//	proxy:   -500_000_000 -> "0.5"    -- the sign silently gone
//
// The second is the worse one: it answers a negative amount with a positive
// number. Both came from taking the fraction of a negative division and
// formatting it as if the sign lived in the whole part — which it does only
// while the absolute value is at least one.
func FormatNanoExact(nano int64) string {
	return FormatScaled(big.NewInt(nano), 9)
}

// FormatScaled renders a scaled integer as a decimal string with no rounding
// and no trailing zeros. `scale` is how many of the digits are fractional, so
// FormatScaled(n, 9) is the nano form.
//
// big.Int rather than int64 because the caller applying several basis-point
// multipliers needs a denominator of 10^(9+4N), and the product overflows an
// int64 long before the result stops being a terminating decimal.
func FormatScaled(n *big.Int, scale int) string {
	if n.Sign() == 0 {
		return "0"
	}
	negative := n.Sign() < 0
	digits := new(big.Int).Abs(n).String()
	if len(digits) <= scale {
		digits = strings.Repeat("0", scale-len(digits)+1) + digits
	}
	cut := len(digits) - scale
	whole, fraction := digits[:cut], strings.TrimRight(digits[cut:], "0")
	out := whole
	if fraction != "" {
		out += "." + fraction
	}
	if negative {
		out = "-" + out
	}
	return out
}

// 十进制字面量的两种接受面。
//
// 此前是三份正则，散在三个包里，且**已经漂了**：`^-?\d+(\.\d+)?$`、
// `^(0|[1-9][0-9]*)(\.[0-9]{1,9})?$`、`^(0|[1-9][0-9]*)(\.[0-9]+)?$`。
// 三者的差别不是笔误，是三个不同的判断——但没有一处说得出自己和另外两处差在哪，
// 而第四个需要校验小数的地方会去抄离它最近的那一份（ADR-0196）。
//
// 收成两个具名函数，各自说明它接受什么：
//
//   - IsSignedDecimal 服务**通用**设置值。它不知道那个数是什么意思，所以既不拒绝
//     负号也不拒绝前导零——一个知道得更少的校验器不该假装知道得更多。
//   - IsPlainDecimal 服务**价格与费率**。这些数进账单，故非负、无前导零，并可选
//     地限制小数位——超出存储精度的位数在别处会被静默舍掉，不如在入口拒绝。
//
// 两者都拒绝科学计数法与两侧空白：一个只通过没人复核过的格式到达网关的费率，
// 正是那种最后差三个数量级的数。
var (
	signedDecimalRE = regexp.MustCompile(`^-?\d+(\.\d+)?$`)
	plainDecimalRE  = regexp.MustCompile(`^(0|[1-9][0-9]*)(\.[0-9]+)?$`)
)

// IsSignedDecimal 判定一个可带符号的十进制字面量。
func IsSignedDecimal(s string) bool { return signedDecimalRE.MatchString(s) }

// IsPlainDecimal 判定一个非负、无前导零的十进制字面量。places > 0 时同时限制
// 小数位数；places <= 0 表示不限。
func IsPlainDecimal(s string, places int) bool {
	if !plainDecimalRE.MatchString(s) {
		return false
	}
	if places <= 0 {
		return true
	}
	dot := strings.IndexByte(s, '.')
	return dot < 0 || len(s)-dot-1 <= places
}
