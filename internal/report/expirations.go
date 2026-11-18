package report

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/branonm/options-book/internal/broker"
	"github.com/branonm/options-book/internal/ledger"
	"github.com/branonm/options-book/internal/strategy/wheel"
)

// Unresolved lists the contracts that ran through expiration with no closing
// order and no recorded outcome, in ledger order, each once.
func Unresolved(bk *wheel.Book) []ledger.ContractKey {
	var todo []ledger.ContractKey
	seen := map[string]bool{}
	for _, a := range bk.Accounts() {
		for _, l := range a.Ledger.Legs {
			if l.End == ledger.EndExpired && !l.PreCampaign && !seen[l.Key.String()] {
				seen[l.Key.String()] = true
				todo = append(todo, l.Key)
			}
		}
	}
	return todo
}

// NothingUnresolved is what `ob expirations` prints when there is nothing to record.
const NothingUnresolved = "Every contract that ran through expiration has a recorded outcome.\n"

// ExpirationPlan prints the outcomes still to record and the bars call that
// resolves them. next says how to record them once the bars are saved.
func ExpirationPlan(w io.Writer, todo []ledger.ContractKey, next string) {
	fmt.Fprintf(w, "%d expiration outcome(s) to record:\n", len(todo))
	bySym := map[string][]string{}
	for _, k := range todo {
		fmt.Fprintf(w, "  %s\n", k)
		bySym[k.Symbol] = append(bySym[k.Symbol], k.Expiry)
	}
	var syms []string
	for s := range bySym {
		syms = append(syms, s)
	}
	sort.Strings(syms)
	first, last := todo[0].Expiry, todo[0].Expiry
	for _, k := range todo {
		if k.Expiry < first {
			first = k.Expiry
		}
		if k.Expiry > last {
			last = k.Expiry
		}
	}
	end, _ := time.Parse("2006-01-02", last)
	fmt.Fprintf(w, "Fetch: get_equity_historicals(symbols=[%s], start_time=%sT00:00:00Z, end_time=%sT00:00:00Z, interval=day, adjustment_type=none)\n",
		strings.Join(syms, ","), first, end.AddDate(0, 0, 1).Format("2006-01-02"))
	fmt.Fprintln(w, next)
}

// NewExpirations reads each unresolved contract's underlying close from saved
// regular-session day bars and returns the outcome records, printing one line
// per contract. bars names the bars file as it is cited in each record's source.
func NewExpirations(w io.Writer, todo []ledger.ContractKey, series []broker.BarSeries, bars string) ([]ledger.Expiration, error) {
	closes := map[string]decimal.Decimal{} // symbol|date
	for _, r := range series {
		if r.Interval != "" && r.Interval != "day" {
			return nil, fmt.Errorf("%s: %s bars are %q; expiration outcomes need day bars", bars, r.Symbol, r.Interval)
		}
		if r.Bounds != "" && r.Bounds != "regular" {
			return nil, fmt.Errorf("%s: %s bars are %q; use regular-session bars", bars, r.Symbol, r.Bounds)
		}
		for _, b := range r.Bars {
			if b.Interpolated {
				continue
			}
			v, err := decimal.NewFromString(b.Close.Text)
			if err != nil {
				return nil, fmt.Errorf("%s: %s %s close: %w", bars, r.Symbol, b.Start, err)
			}
			closes[r.Symbol+"|"+b.Start[:10]] = v
		}
	}
	var out []ledger.Expiration
	for _, k := range todo {
		cl, ok := closes[k.Symbol+"|"+k.Expiry]
		if !ok {
			fmt.Fprintf(w, "  %s: no %s day bar for %s in %s; still unresolved\n", k, k.Symbol, k.Expiry, bars)
			continue
		}
		e, err := ledger.NewExpiration(k, cl, fmt.Sprintf("%s day bar %s (%s)", k.Symbol, k.Expiry, bars))
		if err != nil {
			return nil, err
		}
		out = append(out, e)
		fmt.Fprintf(w, "  %s: %s closed %s vs %s strike → %s\n", k, k.Symbol, cl, k.Strike, e.Outcome)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("nothing recorded")
	}
	return out, nil
}
