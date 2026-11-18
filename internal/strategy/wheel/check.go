package wheel

import (
	"fmt"
	"github.com/branonm/options-book/internal/layout"
	"slices"
	"strings"

	"github.com/shopspring/decimal"

	"github.com/branonm/options-book/internal/broker"
	"github.com/branonm/options-book/internal/ledger"
	"github.com/branonm/options-book/internal/rules"
)

// Severity of a finding.
const (
	Defect = "defect" // the book is wrong and needs an order changed
	Alert  = "alert"  // a rule trigger or a limitation; needs judgment, not a fix
)

// Finding is one check result that did not pass.
type Finding struct {
	Severity string `json:"severity"`
	Check    string `json:"check"`
	Rule     string `json:"rule,omitempty"`
	Account  string `json:"account,omitempty"`
	Subject  string `json:"subject"`
	Expected string `json:"expected,omitempty"`
	Actual   string `json:"actual,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

func (f Finding) String() string {
	s := fmt.Sprintf("[%s] %s %s: %s", f.Check, f.Account, f.Subject, f.Detail)
	if f.Expected != "" || f.Actual != "" {
		s += fmt.Sprintf(" (expected %s, actual %s)", f.Expected, f.Actual)
	}
	return s
}

// Report is the result of checking the whole book.
type Report struct {
	Defects []Finding `json:"defects"`
	Alerts  []Finding `json:"alerts"`
	Passed  int       `json:"checks_passed"`
}

func (r *Report) add(fs []Finding, passed int) {
	for _, f := range fs {
		if f.Severity == Defect {
			r.Defects = append(r.Defects, f)
		} else {
			r.Alerts = append(r.Alerts, f)
		}
	}
	r.Passed += passed
}

func usd(d decimal.Decimal) string { return "$" + d.StringFixed(2) }
func rule(n int) string            { return fmt.Sprintf("Rule %d", n) }

// ExpectedCloser is the closer Rule 10 requires on this call:
// buy-to-close every open contract at round-to-tick(fraction × blended fill).
func (c *ShortCall) ExpectedCloser(r *rules.Rules) Closer {
	raw := c.Leg.FillPerShare.Mul(r.CoveredCallCloser.FractionOfFill)
	return Closer{Qty: c.Leg.OpenQty, Price: r.RoundToTick(raw), Tif: r.CoveredCallCloser.TimeInForce}
}

// DTE is calendar days from asOf to expiration.
func (c *ShortCall) DTE(bk *Book) int {
	return int(c.Expiry.Sub(bk.AsOf).Hours() / 24)
}

// ITM reports whether the call is in the money at the snapshot quote.
func (c *ShortCall) ITM() (itm, known bool) {
	if c.block == nil || !c.block.HasPrice {
		return false, false
	}
	return c.block.Price.GreaterThan(c.Strike), true
}

// Check compares this call's closer and position with what the rules require.
func (c *ShortCall) Check(r *rules.Rules, a *Account, bk *Book) (fs []Finding, passed int) {
	subj := c.Leg.Key.String()
	f := func(sev, check, rl, exp, act, detail string) {
		fs = append(fs, Finding{Severity: sev, Check: check, Rule: rl, Account: a.Name, Subject: subj, Expected: exp, Actual: act, Detail: detail})
	}
	want := c.ExpectedCloser(r)
	r10 := rule(r.CoveredCallCloser.Rule)

	if a.openOrdersKnown {
		if len(c.Closers) == 0 {
			f(Defect, "closer_missing", r10, fmt.Sprintf("buy x%d @ %s %s", want.Qty, usd(want.Price), want.Tif), "none", "short call has no closer")
		} else {
			passed++
			var qty int64
			for _, cl := range c.Closers {
				qty += cl.Qty
				if cl.Tif != want.Tif {
					f(Defect, "closer_tif", r10, want.Tif, cl.Tif, "closer time in force")
				} else {
					passed++
				}
				if !cl.Price.Equal(want.Price) {
					f(Defect, "closer_price", r10, usd(want.Price), usd(cl.Price),
						fmt.Sprintf("closer must be %s × fill $%s", r.CoveredCallCloser.FractionOfFill, c.Leg.FillPerShare.StringFixed(4)))
				} else {
					passed++
				}
			}
			if qty != want.Qty {
				f(Defect, "closer_qty", r10, fmt.Sprint(want.Qty), fmt.Sprint(qty), "closer contracts must equal open contracts")
			} else {
				passed++
			}
			if len(c.Closers) > 1 {
				f(Alert, "closer_split", r10, "1 order", fmt.Sprintf("%d orders", len(c.Closers)), "more than one closer on the same contract")
			}
		}
	}

	pf, pp := checkPosition(a, subj, c.Leg.OpenQty, c.Leg.Net, c.Position, closerQty(c.Closers))
	fs, passed = append(fs, pf...), passed+pp

	if itm, known := c.ITM(); known {
		dte := c.DTE(bk)
		if itm && dte <= r.RollTrigger.ITMAndMaxDTE {
			f(Alert, "roll_trigger", rule(r.RollTrigger.Rule), fmt.Sprintf("OTM or > %d DTE", r.RollTrigger.ITMAndMaxDTE),
				fmt.Sprintf("ITM (%s vs $%s strike), %d DTE", usd(c.block.Price), c.Strike, dte), "Rule 3 trigger: roll for a net credit or let assign")
		} else {
			passed++
		}
	}
	return fs, passed
}

// Check enforces Rule 4a: an acquisition put has no closer.
func (p *ShortPut) Check(r *rules.Rules, a *Account) (fs []Finding, passed int) {
	subj := p.Leg.Key.String()
	if a.openOrdersKnown {
		if len(p.Closers) > 0 && !r.AcquisitionPut.CloserAllowed {
			fs = append(fs, Finding{Severity: Defect, Check: "acquisition_put_closer", Rule: rule(r.AcquisitionPut.Rule), Account: a.Name, Subject: subj,
				Expected: "no closer", Actual: fmt.Sprintf("%d closer(s)", len(p.Closers)),
				Detail: "an acquisition put is held to assignment; label it as an income put if a closer is intended"})
		} else {
			passed++
		}
	}
	pf, pp := checkPosition(a, subj, p.Leg.OpenQty, p.Leg.Net, p.Position, closerQty(p.Closers))
	return append(fs, pf...), passed + pp
}

func closerQty(cs []Closer) int64 {
	var n int64
	for _, c := range cs {
		n += c.Qty
	}
	return n
}

// checkPosition reconciles an open leg from the order record with
// Robinhood's position list. The order record wins (positions lag fills), so
// mismatches are defects in the data to investigate, not in the book.
func checkPosition(a *Account, subj string, qty int64, net decimal.Decimal, pos *broker.OptionPosition, closers int64) (fs []Finding, passed int) {
	if !a.positionsKnown {
		return nil, 0
	}
	f := func(sev, check, exp, act, detail string) {
		fs = append(fs, Finding{Severity: sev, Check: check, Account: a.Name, Subject: subj, Expected: exp, Actual: act, Detail: detail})
	}
	if pos == nil {
		f(Defect, "position_missing", fmt.Sprintf("short x%d", qty), "none", "order record shows this leg open; positions do not (positions can lag fills)")
		return fs, 0
	}
	pq, err1 := decimal.NewFromString(pos.Quantity.Text)
	ap, err2 := decimal.NewFromString(pos.AveragePrice.Text)
	// An empty pending_buy_quantity means the source does not report it (Alpaca's
	// positions carry none); that is "unknown", never 0, so the lag check below
	// is skipped rather than compared against a made-up zero.
	pendingKnown := strings.TrimSpace(pos.PendingBuyQuantity.Text) != ""
	var pb decimal.Decimal
	var err3 error
	if pendingKnown {
		pb, err3 = decimal.NewFromString(pos.PendingBuyQuantity.Text)
	}
	if err1 != nil || err2 != nil || err3 != nil {
		f(Defect, "position_unreadable", "", "", "option position has unparseable fields")
		return fs, 0
	}
	if pos.Side != broker.Short || !pq.Equal(decimal.NewFromInt(qty)) {
		f(Defect, "position_qty", fmt.Sprintf("short x%d", qty), fmt.Sprintf("%s x%s", pos.Side, pos.Quantity.Text), "position quantity disagrees with the order record")
	} else {
		passed++
	}
	// Robinhood's average_price is per contract, negative for a credit, and
	// rounded to 4 places (a $229 credit on 3 contracts reads -76.3333), so
	// compare the total at cents.
	if got := ap.Neg().Mul(decimal.NewFromInt(qty)).Round(2); !got.Equal(net) {
		f(Defect, "position_credit", usd(net), usd(got), "Robinhood average price × contracts disagrees with the ledger's open credit")
	} else {
		passed++
	}
	if !pendingKnown {
		// Not counted as passed: the check could not run.
	} else if !pb.Equal(decimal.NewFromInt(closers)) {
		f(Alert, "pending_buy_lag", fmt.Sprint(closers), pos.PendingBuyQuantity.Text, "pending_buy_quantity disagrees with the open closers (positions can lag orders)")
	} else {
		passed++
	}
	return fs, passed
}

// Check enforces contracts = shares ÷ 100 on the block, then checks each call.
func (b *Block) Check(r *rules.Rules, a *Account, bk *Book) (fs []Finding, passed int) {
	var written int64
	for _, c := range b.Calls {
		written += c.Leg.OpenQty
		cf, cp := c.Check(r, a, bk)
		fs, passed = append(fs, cf...), passed+cp
	}
	lots := b.Shares / r.LotSize.SharesPerContract
	subj := fmt.Sprintf("%s %d sh", b.Symbol, b.Shares)
	switch {
	case written > lots:
		fs = append(fs, Finding{Severity: Defect, Check: "calls_exceed_shares", Rule: "Hard constraint 1", Account: a.Name, Subject: subj,
			Expected: fmt.Sprintf("≤ %d contracts", lots), Actual: fmt.Sprintf("%d contracts", written), Detail: "more calls written than shares cover (naked in an IRA)"})
	case written < lots:
		fs = append(fs, Finding{Severity: Alert, Check: "uncovered_shares", Rule: "Rule 1", Account: a.Name, Subject: subj,
			Expected: fmt.Sprintf("%d contracts", lots), Actual: fmt.Sprintf("%d contracts", written), Detail: "shares not written against; re-cover"})
	default:
		passed++
	}
	return fs, passed
}

// Check runs every check for the account.
func (a *Account) Check(r *rules.Rules, bk *Book) (fs []Finding, passed int) {
	acct := func(sev, check, rl, subj, exp, act, detail string) {
		fs = append(fs, Finding{Severity: sev, Check: check, Rule: rl, Account: a.Name, Subject: subj, Expected: exp, Actual: act, Detail: detail})
	}
	if !a.openOrdersKnown {
		acct(Alert, "input_missing", "", "open orders", layout.OptionOrdersAll, "fills only", "closer checks skipped")
	}
	if !a.positionsKnown {
		acct(Alert, "input_missing", "", "option positions", layout.OptionPositions, "absent", "position reconciliation skipped")
	}
	for _, w := range a.Ledger.Warnings {
		acct(Alert, "ledger_warning", "", "ledger", "", "", w)
	}
	for _, l := range a.Ledger.Legs {
		if l.End == ledger.EndExpired && !l.PreCampaign {
			acct(Defect, "expiration_unresolved", "Rule 4a / Rule 11", l.Key.String(), "outcome recorded", "unknown",
				"record the underlying's close on the expiration date with `ob expirations`; until then the leg is classified provisionally")
		}
	}

	held := map[string]bool{}
	for _, b := range a.Blocks {
		held[b.Symbol] = true
		if !b.HasPrice && bk.quotes {
			acct(Alert, "quote_missing", "", b.Symbol, "a quote", "none", "ITM and concentration checks for this block are incomplete")
		}
		bf, bp := b.Check(r, a, bk)
		fs, passed = append(fs, bf...), passed+bp
	}
	for _, p := range a.Puts {
		pf, pp := p.Check(r, a)
		fs, passed = append(fs, pf...), passed+pp
	}
	for _, c := range a.Uncovered {
		acct(Defect, "naked_call", "Hard constraint 1", c.Leg.Key.String(), "shares held", "no shares", "short call with no shares in the account")
	}
	for _, o := range a.Orphans {
		acct(Defect, "orphan_position", "", fmt.Sprintf("%s %s x%s", o.Symbol, o.Expiry, o.Quantity.Text),
			"an open leg in the order record", "none", "Robinhood holds an option the order history does not explain")
	}
	if a.Expected != nil {
		for s := range held {
			if !slices.Contains(a.Expected, s) {
				acct(Defect, "unexpected_symbol", "Part D check-in 3", s, "not held", "held", "symbol is not in the book's expected list (config/book.json)")
			} else {
				passed++
			}
		}
		for _, s := range a.Expected {
			if !held[s] {
				acct(Defect, "missing_symbol", "Part D check-in 3", s, "held", "not held", "expected symbol is not in the account (sold, assigned, or a phantom in the docs)")
			}
		}
	}
	return fs, passed
}

// Check enforces each concentration cap for the pool.
func (p *Pool) Check(r *rules.Rules, bk *Book) (fs []Finding, passed int) {
	if !bk.quotes {
		return nil, 0
	}
	for _, c := range r.ConcentrationCaps {
		if f, ok := capFinding(c, "pool "+p.Name, p.CapShare); ok {
			fs = append(fs, f)
		} else {
			passed++
		}
	}
	return fs, passed
}

func capFinding(c rules.ConcentrationCap, scope string, share func(rules.ConcentrationCap) (decimal.Decimal, decimal.Decimal)) (Finding, bool) {
	g, t := share(c)
	if t.IsZero() {
		return Finding{}, false
	}
	pct := g.Div(t)
	if pct.LessThanOrEqual(c.MaxShare) {
		return Finding{}, false
	}
	rl := ""
	if c.Rule != 0 {
		rl = rule(c.Rule)
	}
	return Finding{Severity: Alert, Check: "concentration_cap", Rule: rl, Subject: scope + " (" + c.Name + ")",
		Expected: "≤ " + c.MaxShare.Shift(2).StringFixed(1) + "%", Actual: pct.Shift(2).StringFixed(1) + "%",
		Detail: fmt.Sprintf("%s %s of %s share value; no %s puts here", c.Name, usd(g), usd(t), strings.Join(c.Symbols, "/"))}, true
}

// CapShare is the cap group's value ÷ share value across the whole book.
func (bk *Book) CapShare(c rules.ConcentrationCap) (group, total decimal.Decimal) {
	for _, p := range bk.Pools {
		g, t := p.CapShare(c)
		group, total = group.Add(g), total.Add(t)
	}
	return group, total
}

// Check runs every check in the book.
func (bk *Book) Check(r *rules.Rules) Report {
	var rep Report
	for _, p := range bk.Pools {
		for _, a := range p.Accounts {
			rep.add(a.Check(r, bk))
		}
		rep.add(p.Check(r, bk))
	}
	if bk.quotes {
		for _, c := range r.ConcentrationCaps {
			if f, ok := capFinding(c, "book-wide", bk.CapShare); ok {
				rep.add([]Finding{f}, 0)
			} else {
				rep.add(nil, 1)
			}
		}
	}
	return rep
}
