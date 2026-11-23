package wheel

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/branonm/options-book/internal/ledger"
	"github.com/branonm/options-book/internal/rules"
	"github.com/branonm/options-book/internal/snapshot"
)

// Trade kinds a ticket can propose.
const (
	TradeCoveredCall    = "covered_call"
	TradeAcquisitionPut = "acquisition_put"
	TradeIncomePut      = "income_put"
)

// Proposal is a trade to evaluate. The tools never place it.
type Proposal struct {
	Account string
	Kind    string
	Option  *snapshot.Option
	Limit   decimal.Decimal
	Qty     int64 // 0 = every free lot (calls) or 1 (puts)
	// FetchedAt is when the server itself fetched the quote (ticket by contract,
	// PLAN.md addendum A). When set, the quote window is checked against it,
	// never against a time a session supplies; zero keeps the saved-snapshot rule.
	FetchedAt time.Time
}

// CallAway is what assignment at the strike would pay, as separate components.
type CallAway struct {
	SharesCalled int64           `json:"shares_called"`
	ShareGain    decimal.Decimal `json:"share_gain_at_strike"`     // (strike − basis) × shares called
	Premium      decimal.Decimal `json:"block_premium_attributed"` // block premium after this credit, pro rata to shares called
	Total        decimal.Decimal `json:"total"`
	Days         int             `json:"days"`       // calendar days to expiration
	Capital      decimal.Decimal `json:"capital"`    // basis × shares called
	Annualized   decimal.Decimal `json:"annualized"` // total ÷ capital × 365 ÷ days
}

// Ticket is a fully evaluated proposal.
type Ticket struct {
	Account  string          `json:"account"`
	Kind     string          `json:"kind"`
	Contract string          `json:"contract"`
	OptionID string          `json:"option_id"`
	Symbol   string          `json:"symbol"`
	Strike   decimal.Decimal `json:"strike"`
	Expiry   string          `json:"expiry"`
	DTE      int             `json:"dte"`
	Qty      int64           `json:"qty"`
	Limit    decimal.Decimal `json:"limit"`
	Credit   decimal.Decimal `json:"credit"`

	Bid       decimal.Decimal `json:"bid"`
	Ask       decimal.Decimal `json:"ask"`
	Mark      decimal.Decimal `json:"mark"`
	Delta     decimal.Decimal `json:"delta"`
	OI        int64           `json:"open_interest"`
	QuoteTime string          `json:"quote_time"`
	FetchedAt string          `json:"fetched_at,omitempty"` // set when the server fetched the quote itself

	// Covered calls.
	Basis              *decimal.Decimal `json:"basis,omitempty"`
	Spot               *decimal.Decimal `json:"spot,omitempty"`
	BreakevenBefore    *decimal.Decimal `json:"breakeven_before,omitempty"`
	BreakevenAfter     *decimal.Decimal `json:"breakeven_after,omitempty"`
	Clearance          *decimal.Decimal `json:"clearance,omitempty"`     // strike − breakeven after, $/share
	ClearancePct       *decimal.Decimal `json:"clearance_pct,omitempty"` // ÷ breakeven after
	RequiredClearance  *decimal.Decimal `json:"required_clearance_pct,omitempty"`
	DepthUnderwaterPct *decimal.Decimal `json:"depth_underwater_pct,omitempty"`
	CallAway           *CallAway        `json:"call_away,omitempty"`

	// Puts.
	EffectiveCost *decimal.Decimal `json:"effective_cost,omitempty"` // strike − limit
	CashRequired  *decimal.Decimal `json:"cash_required,omitempty"`
	BuyingPower   *decimal.Decimal `json:"buying_power,omitempty"`

	MaxLoss          decimal.Decimal `json:"max_loss"`
	MaxLossPctPool   decimal.Decimal `json:"max_loss_pct_pool"`
	PoolEquity       decimal.Decimal `json:"pool_equity"`
	PoolEquitySource string          `json:"pool_equity_source"`

	Closer     *Closer `json:"closer,omitempty"`
	CloserRule string  `json:"closer_rule,omitempty"`

	Rejections []string `json:"rejections"`
	Warnings   []string `json:"warnings"`
	Accepted   bool     `json:"accepted"`

	// kinds[i] classifies Rejections[i] ("" = unclassified); Recommend tallies by it.
	kinds []string
}

// Rejection kinds: stable labels for the reasons Recommend tallies, so a
// reworded message can't change them.
const (
	KindNotTradable  = "contract not tradable"
	KindExpired      = "expired"
	KindOpenInterest = "open interest below the minimum"
	KindBreakeven    = "strike below the breakeven (Rule 11)"
	KindClearance    = "clearance below the graded minimum (Rule 11)"
	KindDeltaAbove   = "delta above the band"
	KindQuote        = "quote outside the trading window or stale"
)

func (t *Ticket) reject(f string, a ...any) { t.rejectAs("", f, a...) }
func (t *Ticket) rejectAs(kind, f string, a ...any) {
	t.Rejections = append(t.Rejections, fmt.Sprintf(f, a...))
	t.kinds = append(t.kinds, kind)
}

// RejectionKind is the kind of the i-th rejection, or "" if unclassified.
func (t *Ticket) RejectionKind(i int) string {
	if i < 0 || i >= len(t.kinds) {
		return ""
	}
	return t.kinds[i]
}
func (t *Ticket) warn(f string, a ...any)   { t.Warnings = append(t.Warnings, fmt.Sprintf(f, a...)) }
func dp(d decimal.Decimal) *decimal.Decimal { return &d }

// Account finds an account by name.
func (bk *Book) Account(name string) (*Account, *Pool) {
	for _, p := range bk.Pools {
		for _, a := range p.Accounts {
			if a.Name == name {
				return a, p
			}
		}
	}
	return nil, nil
}

// Equity is the pool's account value from get_portfolio, falling back to share
// value at snapshot quotes when any account's portfolio was not saved.
func (p *Pool) Equity() (decimal.Decimal, string) {
	total := decimal.Zero
	for _, a := range p.Accounts {
		if a.portfolio == nil {
			v := decimal.Zero
			for _, x := range p.Accounts {
				v = v.Add(x.Value())
			}
			return v, "share value at snapshot quotes (portfolio.json missing)"
		}
		tv, err := decimal.NewFromString(a.portfolio.TotalValue.Text)
		if err != nil {
			return decimal.Zero, "unreadable portfolio total_value"
		}
		total = total.Add(tv)
	}
	return total, "get_portfolio total_value"
}

// Ticket evaluates a proposal against the book and the rules.
func (bk *Book) Ticket(r *rules.Rules, p Proposal) (*Ticket, error) {
	a, pool := bk.Account(p.Account)
	if a == nil {
		return nil, fmt.Errorf("no account %q in the book", p.Account)
	}
	in, q := p.Option.Instrument, p.Option.Quote
	strike, err := decimal.NewFromString(in.Strike.Text)
	if err != nil {
		return nil, fmt.Errorf("instrument strike: %w", err)
	}
	exp, err := time.Parse("2006-01-02", in.Expiry)
	if err != nil {
		return nil, fmt.Errorf("instrument expiration: %w", err)
	}
	num := func(s string) decimal.Decimal { v, _ := decimal.NewFromString(s); return v }

	t := &Ticket{
		Account: a.Name, Kind: p.Kind, OptionID: in.ID, Symbol: in.Symbol, Strike: strike,
		Expiry: in.Expiry, DTE: int(exp.Sub(bk.AsOf).Hours() / 24), Limit: p.Limit,
		Bid: num(q.Bid.Text), Ask: num(q.Ask.Text), Mark: num(q.Mark.Text), Delta: num(q.Delta.Text).Abs(),
		OI: q.OpenInterest, QuoteTime: q.UpdatedAt,
		Rejections: []string{}, Warnings: []string{},
	}
	t.Contract = ledger.ContractKey{Symbol: in.Symbol, Type: in.Right, Strike: strike.String(), Expiry: in.Expiry}.String()
	mult := decimal.NewFromInt(r.LotSize.SharesPerContract)

	// Contract and kind agree.
	wantType := map[string]string{TradeCoveredCall: "call", TradeAcquisitionPut: "put", TradeIncomePut: "put"}[p.Kind]
	switch {
	case wantType == "":
		return nil, fmt.Errorf("unknown trade kind %q (covered_call, acquisition_put, income_put)", p.Kind)
	case in.Right != wantType:
		return nil, fmt.Errorf("%s needs a %s; instrument %s is a %s", p.Kind, wantType, in.ID, in.Right)
	}

	// Contract state and market data.
	if in.State != "active" || in.Tradability != "tradable" {
		t.rejectAs(KindNotTradable, "contract is %s/%s, not active/tradable", in.State, in.Tradability)
	}
	if t.DTE <= 0 {
		t.rejectAs(KindExpired, "contract expires %s, on or before the as-of date", in.Expiry)
	}
	if p.FetchedAt.IsZero() {
		checkQuoteTime(t, r, bk.AsOf)
	} else {
		t.FetchedAt = p.FetchedAt.UTC().Format(time.RFC3339)
		checkFetchTime(t, r, bk.AsOf, p.FetchedAt)
	}
	if t.OI < r.MinOpenInterest.Value {
		t.rejectAs(KindOpenInterest, "open interest %d is below %d (%s)", t.OI, r.MinOpenInterest.Value, r.MinOpenInterest.Rule)
	}
	if !p.Limit.IsPositive() {
		t.reject("limit must be > 0")
	}
	if t.Bid.IsPositive() && p.Limit.LessThan(t.Bid) {
		t.reject("limit $%s is below the bid $%s (Part D: don't sell below the bid)", p.Limit.StringFixed(2), t.Bid.StringFixed(2))
	}
	if tick := instrumentTick(in.Ticks.Cutoff.Text, in.Ticks.Below.Text, in.Ticks.Above.Text, p.Limit); tick.IsPositive() && !p.Limit.Mod(tick).IsZero() {
		t.reject("limit $%s is not on the contract's $%s tick", p.Limit.String(), tick.String())
	}
	if !ticksMatch(r, in.Ticks.Cutoff.Text, in.Ticks.Below.Text, in.Ticks.Above.Text) {
		t.warn("contract ticks (%s below $%s, %s above) differ from rules.json option_ticks; closer rounding uses rules.json",
			in.Ticks.Below.Text, in.Ticks.Cutoff.Text, in.Ticks.Above.Text)
	}

	equity, src := pool.Equity()
	t.PoolEquity, t.PoolEquitySource = equity, src

	switch p.Kind {
	case TradeCoveredCall:
		ticketCall(t, r, a, p, strike, mult)
	default:
		ticketPut(t, r, a, pool, bk, p, strike, mult)
	}
	if t.PoolEquity.IsPositive() {
		t.MaxLossPctPool = t.MaxLoss.Div(t.PoolEquity)
	}
	t.Accepted = len(t.Rejections) == 0
	return t, nil
}

func ticketCall(t *Ticket, r *rules.Rules, a *Account, p Proposal, strike, mult decimal.Decimal) {
	var blk *Block
	for _, b := range a.Blocks {
		if b.Symbol == t.Symbol {
			blk = b
		}
	}
	if blk == nil {
		t.reject("no %s shares in %s: a call here would be naked (Hard constraint 1)", t.Symbol, a.Name)
		return
	}
	var written int64
	for _, c := range blk.Calls {
		written += c.Leg.OpenQty
	}
	free := blk.Shares/r.LotSize.SharesPerContract - written
	t.Qty = p.Qty
	if t.Qty == 0 {
		t.Qty = free
	}
	if free <= 0 {
		t.reject("%s %d shares already carry %d contracts; no free lots", t.Symbol, blk.Shares, written)
		t.Qty = 0
		return
	}
	if t.Qty > free {
		t.reject("%d contracts requested; only %d free lots (%d shares, %d written)", t.Qty, free, blk.Shares, written)
	}
	t.Credit = p.Limit.Mul(decimal.NewFromInt(t.Qty)).Mul(mult)

	// Rule 11: breakeven after this credit; strike must be at or above it.
	shares := decimal.NewFromInt(blk.Shares)
	beAfter := shares.Mul(blk.Basis).Sub(blk.Premium).Sub(t.Credit).DivRound(shares, ledger.DivPlaces)
	t.Basis, t.BreakevenBefore, t.BreakevenAfter = dp(blk.Basis), dp(blk.Breakeven), dp(beAfter)
	clr := strike.Sub(beAfter)
	t.Clearance = dp(clr)
	t.ClearancePct = dp(clr.DivRound(beAfter, ledger.DivPlaces))
	if strike.LessThan(beAfter) {
		t.rejectAs(KindBreakeven, "strike $%s is below the premium-adjusted breakeven $%s (Rule 11)", strike, beAfter.StringFixed(2))
	}

	// Graded clearance needs a spot price.
	if !blk.HasPrice {
		t.reject("no %s quote in the snapshot; graded clearance (Rule 11) cannot be checked", t.Symbol)
	} else {
		t.Spot = dp(blk.Price)
		depth := blk.Basis.Sub(blk.Price).DivRound(blk.Basis, ledger.DivPlaces)
		req := r.RequiredClearance(depth)
		t.DepthUnderwaterPct, t.RequiredClearance = dp(depth), dp(req)
		g := r.GradedClearance
		switch {
		case t.ClearancePct.LessThan(req):
			t.rejectAs(KindClearance, "clearance %s%% is below the %s%% graded-clearance minimum at %s%% underwater (Rule 11)",
				pct(*t.ClearancePct), pct(req), pct(depth))
		case depth.LessThanOrEqual(g.NearBasisDepth) && t.ClearancePct.LessThan(g.NearBasisTarget):
			t.warn("clearance %s%% meets the %s%% minimum but is under the %s%% target near basis (Rule 11)",
				pct(*t.ClearancePct), pct(g.NearBasisMin), pct(g.NearBasisTarget))
		}
	}

	// Delta band. Rule 9: when the breakeven floor forces a lower delta, the floor wins.
	b := r.DeltaBands.CoveredCall
	switch {
	case t.Delta.GreaterThan(b.Max):
		t.rejectAs(KindDeltaAbove, "delta %s is above the %s–%s band (%s)", t.Delta.StringFixed(3), b.Min, b.Max, b.Rule)
	case t.Delta.LessThan(b.Min):
		t.warn("delta %s is below the %s–%s band; acceptable only if the Rule 11 floor forces this strike (Rule 9)", t.Delta.StringFixed(3), b.Min, b.Max)
	}

	// Call-away payoff, as components.
	called := t.Qty * r.LotSize.SharesPerContract
	t.CallAway = callAway(strike, blk.Basis, blk.Premium.Add(t.Credit), blk.Shares, called, t.DTE)

	// Rule 6: covered call max loss = share basis × 100 − premium, per contract.
	t.MaxLoss = blk.Basis.Mul(decimal.NewFromInt(called)).Sub(t.Credit)

	// Rule 10 closer, from the same method the check-in uses.
	sc := &ShortCall{Leg: ledger.Leg{OpenQty: t.Qty, FillPerShare: p.Limit}, Strike: strike, block: blk}
	cl := sc.ExpectedCloser(r)
	t.Closer, t.CloserRule = &cl, fmt.Sprintf("Rule %d: %s × fill, %s. Recompute off the actual fill once it fills", r.CoveredCallCloser.Rule, r.CoveredCallCloser.FractionOfFill, cl.Tif)
}

func ticketPut(t *Ticket, r *rules.Rules, a *Account, pool *Pool, bk *Book, p Proposal, strike, mult decimal.Decimal) {
	t.Qty = p.Qty
	if t.Qty == 0 {
		t.Qty = 1
	}
	qty := decimal.NewFromInt(t.Qty)
	t.Credit = p.Limit.Mul(qty).Mul(mult)
	t.EffectiveCost = dp(strike.Sub(p.Limit))
	cash := strike.Mul(qty).Mul(mult)
	t.CashRequired = dp(cash)
	t.MaxLoss = cash.Sub(t.Credit) // Rule 6: strike × 100 − premium

	// Hard constraint 5: size against settled cash.
	if a.portfolio == nil {
		t.reject("portfolio.json missing for %s: cannot confirm the put is cash-secured (Hard constraint 5)", a.Name)
	} else if bp, err := decimal.NewFromString(a.portfolio.BuyingPower.Text); err != nil {
		t.reject("unreadable buying power for %s", a.Name)
	} else {
		t.BuyingPower = dp(bp)
		if cash.GreaterThan(bp) {
			t.reject("needs $%s of collateral; %s buying power is $%s (Hard constraint 5)", cash.StringFixed(2), a.Name, bp.StringFixed(2))
		}
	}

	// Concentration caps: no new puts in a group's symbols in a pool over its cap.
	if bk.quotes {
		for _, c := range r.CapsFor(t.Symbol) {
			g, tot := pool.CapShare(c)
			if tot.IsPositive() && g.Div(tot).GreaterThan(c.MaxShare) {
				t.reject("pool %s is %s%% %s, over the %s%% cap: no %s puts here%s",
					pool.Name, pct(g.Div(tot)), c.Name, pct(c.MaxShare), t.Symbol, capRule(c))
			}
		}
	}

	// Rule 5: a new put is new risk.
	if active, why := pool.Cooldown(r, bk.AsOf); active {
		t.reject("Rule %d cooldown active in pool %s: %s", r.Cooldown.Rule, pool.Name, why)
	}

	band := r.DeltaBands.AcquisitionPut
	if p.Kind == TradeIncomePut {
		band = r.DeltaBands.IncomePut
	}
	switch {
	case t.Delta.GreaterThan(band.Max):
		t.reject("delta %s is above the %s–%s band (%s)", t.Delta.StringFixed(3), band.Min, band.Max, band.Rule)
	case t.Delta.LessThan(band.Min):
		t.warn("delta %s is below the %s–%s band (%s)", t.Delta.StringFixed(3), band.Min, band.Max, band.Rule)
	}

	if p.Kind == TradeIncomePut {
		cl := Closer{Qty: t.Qty, Price: r.RoundToTick(p.Limit.Mul(r.IncomePutCloser.FractionOfFill)), Tif: r.IncomePutCloser.TimeInForce}
		t.Closer = &cl
		t.CloserRule = fmt.Sprintf("%s: income put closer %s × fill, %s. Recompute off the actual fill", r.IncomePutCloser.Rule, r.IncomePutCloser.FractionOfFill, cl.Tif)
	} else {
		t.CloserRule = fmt.Sprintf("Rule %d: acquisition put, no closer. Hold to assignment", r.AcquisitionPut.Rule)
	}
}

// checkQuoteTime enforces Hard constraint 7: no opening-bell or after-hours
// quotes, and the quote must be from the as-of session.
func checkQuoteTime(t *Ticket, r *rules.Rules, asOf time.Time) {
	qt, err := time.Parse(time.RFC3339Nano, t.QuoteTime)
	if err != nil {
		t.rejectAs(KindQuote, "quote has no readable updated_at (%q)", t.QuoteTime)
		return
	}
	loc, _ := time.LoadLocation(r.QuoteWindow.Timezone)
	lt := qt.In(loc)
	hm := func(s string) time.Time {
		x, _ := time.ParseInLocation("15:04", s, loc)
		return time.Date(lt.Year(), lt.Month(), lt.Day(), x.Hour(), x.Minute(), 0, 0, loc)
	}
	open := hm(r.QuoteWindow.Open).Add(time.Duration(r.QuoteWindow.ExcludeOpeningMinutes) * time.Minute)
	closeT := hm(r.QuoteWindow.Close)
	stamp := lt.Format("Mon 2006-01-02 15:04 MST")
	switch {
	case lt.Weekday() == time.Saturday || lt.Weekday() == time.Sunday:
		t.rejectAs(KindQuote, "quote is from a weekend (%s): not a price (%s)", stamp, r.QuoteWindow.Rule)
	case lt.Before(open):
		t.rejectAs(KindQuote, "quote is from before %s or the opening bell (%s): not a price (%s)", r.QuoteWindow.Open, stamp, r.QuoteWindow.Rule)
	case !lt.Before(closeT):
		t.rejectAs(KindQuote, "quote is from after the %s close (%s): not a price (%s)", r.QuoteWindow.Close, stamp, r.QuoteWindow.Rule)
	}
	if lt.Format("2006-01-02") != asOf.Format("2006-01-02") {
		t.rejectAs(KindQuote, "quote is from %s, not the as-of session %s: re-quote", lt.Format("2006-01-02"), asOf.Format("2006-01-02"))
	}
}

// Cooldown reports whether Rule 5 is freezing new risk in the pool.
// Sessions are weekdays; exchange holidays are not modelled.
func (p *Pool) Cooldown(r *rules.Rules, asOf time.Time) (bool, string) {
	type loss struct {
		day time.Time
		amt decimal.Decimal
		leg string
	}
	var losses []loss
	for _, a := range p.Accounts {
		for _, l := range a.Ledger.Legs {
			if l.PreCampaign || l.End == ledger.EndOpen || l.Kind == ledger.KindAcquisitionPut || !l.Net.IsNegative() {
				continue
			}
			d, err := time.Parse("2006-01-02", l.ClosedAt)
			if err != nil {
				continue
			}
			losses = append(losses, loss{d, l.Net, a.Name + " " + l.Key.String()})
		}
	}
	sort.Slice(losses, func(i, j int) bool { return losses[i].day.Before(losses[j].day) })
	equity, _ := p.Equity()
	cap := equity.Mul(r.Cooldown.SingleLossShareOfPool)
	for i, l := range losses {
		var trigger string
		if equity.IsPositive() && l.amt.Neg().GreaterThan(cap) {
			trigger = fmt.Sprintf("%s lost $%s, over %s%% of pool equity", l.leg, l.amt.Neg().StringFixed(2), pct(r.Cooldown.SingleLossShareOfPool))
		}
		n, names := 0, []string{}
		for j := 0; j <= i; j++ {
			if sessionsBetween(losses[j].day, l.day) < r.Cooldown.WithinSessions {
				n++
				names = append(names, losses[j].leg)
			}
		}
		if n >= r.Cooldown.Losses && trigger == "" {
			trigger = fmt.Sprintf("%d losses within %d sessions (%s)", n, r.Cooldown.WithinSessions, strings.Join(names, ", "))
		}
		if trigger != "" && sessionsBetween(l.day, asOf) < r.Cooldown.FreezeSessions {
			return true, fmt.Sprintf("%s, triggered %s; freeze runs %d sessions", trigger, l.day.Format("2006-01-02"), r.Cooldown.FreezeSessions)
		}
	}
	return false, ""
}

// sessionsBetween counts weekdays d with a < d <= b.
func sessionsBetween(a, b time.Time) int {
	n := 0
	for d := a.AddDate(0, 0, 1); !d.After(b); d = d.AddDate(0, 0, 1) {
		if d.Weekday() != time.Saturday && d.Weekday() != time.Sunday {
			n++
		}
	}
	return n
}

func instrumentTick(cutoff, below, above string, p decimal.Decimal) decimal.Decimal {
	c, err1 := decimal.NewFromString(cutoff)
	bl, err2 := decimal.NewFromString(below)
	ab, err3 := decimal.NewFromString(above)
	if err1 != nil || err2 != nil || err3 != nil {
		return decimal.Zero
	}
	if p.LessThan(c) {
		return bl
	}
	return ab
}

func ticksMatch(r *rules.Rules, cutoff, below, above string) bool {
	c, err1 := decimal.NewFromString(cutoff)
	bl, err2 := decimal.NewFromString(below)
	ab, err3 := decimal.NewFromString(above)
	if err1 != nil || err2 != nil || err3 != nil {
		return false
	}
	return c.Equal(r.OptionTicks.Cutoff) && bl.Equal(r.OptionTicks.BelowCutoff) && ab.Equal(r.OptionTicks.AtOrAboveCutoff)
}

func pct(d decimal.Decimal) string { return d.Shift(2).StringFixed(1) }

// checkFetchTime is Hard constraint 7 for a quote the server fetched itself:
// the window is checked against the server's own fetch time (no opening
// minutes, no after-hours, no weekend, the as-of session), and the quote
// Robinhood returned must be from that same session, or it's stale.
func checkFetchTime(t *Ticket, r *rules.Rules, asOf, fetched time.Time) {
	loc, _ := time.LoadLocation(r.QuoteWindow.Timezone)
	lt := fetched.In(loc)
	hm := func(s string) time.Time {
		x, _ := time.ParseInLocation("15:04", s, loc)
		return time.Date(lt.Year(), lt.Month(), lt.Day(), x.Hour(), x.Minute(), 0, 0, loc)
	}
	open := hm(r.QuoteWindow.Open).Add(time.Duration(r.QuoteWindow.ExcludeOpeningMinutes) * time.Minute)
	closeT := hm(r.QuoteWindow.Close)
	stamp := lt.Format("Mon 2006-01-02 15:04 MST")
	switch {
	case lt.Weekday() == time.Saturday || lt.Weekday() == time.Sunday:
		t.rejectAs(KindQuote, "quote was fetched on a weekend (%s): not a price (%s)", stamp, r.QuoteWindow.Rule)
	case lt.Before(open):
		t.rejectAs(KindQuote, "quote was fetched before %s plus the first %d minutes (%s): not a price (%s)", r.QuoteWindow.Open, r.QuoteWindow.ExcludeOpeningMinutes, stamp, r.QuoteWindow.Rule)
	case !lt.Before(closeT):
		t.rejectAs(KindQuote, "quote was fetched after the %s close (%s): not a price (%s)", r.QuoteWindow.Close, stamp, r.QuoteWindow.Rule)
	}
	if lt.Format("2006-01-02") != asOf.Format("2006-01-02") {
		t.rejectAs(KindQuote, "quote was fetched in the %s session, not the as-of session %s", lt.Format("2006-01-02"), asOf.Format("2006-01-02"))
	}
	if qt, err := time.Parse(time.RFC3339Nano, t.QuoteTime); err != nil {
		t.rejectAs(KindQuote, "quote has no readable updated_at (%q)", t.QuoteTime)
	} else if q := qt.In(loc).Format("2006-01-02"); q != lt.Format("2006-01-02") {
		t.rejectAs(KindQuote, "Robinhood's quote is from the %s session, not the %s session it was fetched in: stale", q, lt.Format("2006-01-02"))
	}
}

// capRule is the " (Rule N)" suffix for a cap that cites a rule, else "".
func capRule(c rules.ConcentrationCap) string {
	if c.Rule == 0 {
		return ""
	}
	return fmt.Sprintf(" (Rule %d)", c.Rule)
}
