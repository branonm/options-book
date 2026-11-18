// Package occ parses OCC option symbols, the canonical contract key
// (PLAN.md item 6, decision 2): root (up to 6 characters, space-padded to 6
// in the 21-character form), expiry YYMMDD, C or P, and the strike × 1000 in
// 8 digits. "AAA   270115C00076000" is AAA $76 call, 2027-01-15.
package occ

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// Contract is one option contract.
type Contract struct {
	Root   string          // underlying symbol, e.g. AAA
	Expiry string          // YYYY-MM-DD
	Type   string          // call or put
	Strike decimal.Decimal // dollars
}

var pattern = regexp.MustCompile(`^([A-Z][A-Z0-9.]{0,5}) *([0-9]{6})([CP])([0-9]{8})$`)

// Parse reads an OCC symbol, padded or not ("AAA   270115C00076000" or
// "KRE270115C00076000"). Anything else is an error, never a guess.
func Parse(s string) (Contract, error) {
	m := pattern.FindStringSubmatch(strings.ToUpper(strings.TrimSpace(s)))
	if m == nil {
		return Contract{}, fmt.Errorf("%q is not an OCC symbol (e.g. AAA   270115C00076000: root, YYMMDD, C or P, strike × 1000 in 8 digits)", s)
	}
	exp, err := time.Parse("060102", m[2])
	if err != nil {
		return Contract{}, fmt.Errorf("%q: expiry %s is not a date", s, m[2])
	}
	strike := decimal.RequireFromString(m[4]).Shift(-3)
	if !strike.IsPositive() {
		return Contract{}, fmt.Errorf("%q: the strike is zero", s)
	}
	typ := map[string]string{"C": "call", "P": "put"}[m[3]]
	return Contract{Root: m[1], Expiry: exp.Format("2006-01-02"), Type: typ, Strike: strike}, nil
}

// String is the padded 21-character form.
func (c Contract) String() string {
	exp, _ := time.Parse("2006-01-02", c.Expiry)
	return fmt.Sprintf("%-6s%s%s%08s", c.Root, exp.Format("060102"), strings.ToUpper(c.Type[:1]), c.Strike.Shift(3).StringFixed(0))
}
