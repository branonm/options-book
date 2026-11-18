// Package reconcile checks the ledger against Robinhood's own realized P&L,
// an independent source the ledger is not derived from.
//
// Robinhood's option P&L and the wheel ledger define realized premium
// differently in one place: when a covered call is assigned, Robinhood books
// its premium into the share sale, while the wheel counts it as premium
// earned. Assigned puts are excluded by both (the credit is share basis). So
//
//	Robinhood option P&L = ledger realized − premium of calls assigned in the window
package reconcile

import (
	"fmt"
	"strings"

	"github.com/shopspring/decimal"

	"github.com/branonm/options-book/internal/broker"
	"github.com/branonm/options-book/internal/ledger"
)

// Line is one account's reconciliation.
type Line struct {
	Account        string          `json:"account"`
	Window         string          `json:"window"`
	LedgerRealized decimal.Decimal `json:"ledger_realized"`
	AssignedCalls  decimal.Decimal `json:"assigned_call_premium"`
	Expected       decimal.Decimal `json:"expected_robinhood"`
	Robinhood      decimal.Decimal `json:"robinhood"`
	Difference     decimal.Decimal `json:"difference"` // robinhood − expected
	Legs           int             `json:"legs"`
	Excluded       []string        `json:"assigned_calls,omitempty"`
}

// OK reports whether the two sources agree to the dollar. Robinhood reports
// whole dollars, so anything under $1 is rounding.
func (l Line) OK() bool { return l.Difference.Abs().LessThan(decimal.NewFromInt(1)) }

// Account reconciles one account's ledger against a saved get_realized_pnl response.
func Account(l *ledger.AccountLedger, pnl broker.RealizedPnL) (Line, error) {
	from, to, ok := strings.Cut(pnl.Window, "..")
	if !ok || len(from) != 10 || len(to) != 10 {
		return Line{}, fmt.Errorf("window %q is not YYYY-MM-DD..YYYY-MM-DD", pnl.Window)
	}
	rhTotal, err := decimal.NewFromString(pnl.Total.Text)
	if err != nil {
		return Line{}, fmt.Errorf("total_returns %q: %w", pnl.Total.Text, err)
	}
	line := Line{Account: l.Account, Window: pnl.Window, Robinhood: rhTotal}
	for _, lg := range l.Legs {
		if lg.PreCampaign || lg.End == ledger.EndOpen || lg.Kind == ledger.KindAcquisitionPut {
			continue
		}
		if lg.ClosedAt < from || lg.ClosedAt > to {
			continue
		}
		if lg.End == ledger.EndExpired {
			return Line{}, fmt.Errorf("%s: expiration outcome not recorded; run `ob expirations` first", lg.Key)
		}
		line.LedgerRealized = line.LedgerRealized.Add(lg.Net)
		line.Legs++
		if lg.Key.Type == "call" && lg.End == ledger.EndAssigned {
			line.AssignedCalls = line.AssignedCalls.Add(lg.Net)
			line.Excluded = append(line.Excluded, fmt.Sprintf("%s (%s)", lg.Key, lg.Net.StringFixed(2)))
		}
	}
	line.Expected = line.LedgerRealized.Sub(line.AssignedCalls)
	line.Difference = line.Robinhood.Sub(line.Expected)
	return line, nil
}
