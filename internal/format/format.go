// Package format turns decimals into display strings. Rounding happens here
// and only here: half away from zero, at the precision shown.
package format

import (
	"fmt"
	"strings"

	"github.com/shopspring/decimal"
)

// USD formats dollars to cents: "$1,234.56" / "-$1,234.56".
func USD(d decimal.Decimal) string { return money(d, 2) }

// USD0 formats whole dollars: "$1,235" / "-$1,235".
func USD0(d decimal.Decimal) string { return money(d, 0) }

// Signed is USD0 with an explicit plus sign on gains: "+$591" / "-$118".
func Signed(d decimal.Decimal) string {
	s := money(d, 0)
	if d.Round(0).IsPositive() {
		return "+" + s
	}
	return s
}

// Pct formats a fraction as a percentage with one decimal: 0.531 → "53.1%".
func Pct(d decimal.Decimal) string { return d.Shift(2).StringFixed(1) + "%" }

// Price formats a per-share price to cents without separators: "$0.31".
func Price(d decimal.Decimal) string { return "$" + d.StringFixed(2) }

func money(d decimal.Decimal, places int32) string {
	s := d.Abs().StringFixed(places)
	whole, frac, hasFrac := strings.Cut(s, ".")
	var b strings.Builder
	if d.Round(places).IsNegative() {
		b.WriteByte('-')
	}
	b.WriteByte('$')
	for i, r := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if hasFrac {
		b.WriteByte('.')
		b.WriteString(frac)
	}
	return b.String()
}

// SignedPrice formats a per-share difference with its sign in front: "+$0.32" / "-$3.01".
func SignedPrice(d decimal.Decimal) string {
	r := d.Round(2)
	switch {
	case r.IsPositive():
		return "+$" + r.StringFixed(2)
	case r.IsNegative():
		return "-$" + r.Abs().StringFixed(2)
	}
	return "$0.00"
}

// Plural returns "1 alert" / "2 alerts".
func Plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}
