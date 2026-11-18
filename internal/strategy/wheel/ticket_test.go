package wheel

import (
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/branonm/options-book/internal/broker"
	"github.com/branonm/options-book/internal/ledger"
	"github.com/branonm/options-book/internal/snapshot"
)

// Every figure below is invented, and every expectation is worked out by hand
// from the synthetic rules in testdata/synthetic/books/ira-wheel/rules.json:
// a closer is 50% of the fill rounded up to the tick, covered-call delta runs
// 0.20 to 0.40, the graded clearance is 5% at or under 3% underwater and falls
// linearly to 1% at 12% underwater, open interest must be at least 50, quotes
// count from 09:40 to 16:00 ET, and a pool freezes after 3 losses in 10
// sessions (or one loss over 2% of the pool) for 2 sessions.

// A Friday-morning regular-session time, 10:30 ET.
const sessionQuote = "2026-11-06T15:30:00Z"

var nov6 = time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)

func option(sym, typ, strike, exp, bid, ask, delta string, oi int64, at string) *snapshot.Option {
	o := &snapshot.Option{
		Quote: broker.OptionQuote{InstrumentID: "x", Bid: broker.N(bid), Ask: broker.N(ask), Mark: broker.N(ask), Delta: broker.N(delta), OpenInterest: oi, UpdatedAt: at},
		Instrument: broker.Instrument{ID: "x", Symbol: sym, Expiry: exp, Strike: broker.N(strike), Right: typ,
			State: "active", Tradability: "tradable", Multiplier: broker.N("100")},
	}
	o.Instrument.Ticks = broker.Ticks{Above: broker.N("0.05"), Below: broker.N("0.01"), Cutoff: broker.N("3.00")}
	return o
}

// ticketBook is a one-pool book with one block and a portfolio.
func ticketBook(sym string, shares int64, basis, premium, spot, total, bp string) *Book {
	shD := decimal.NewFromInt(shares)
	be := shD.Mul(d(basis)).Sub(d(premium)).DivRound(shD, ledger.DivPlaces)
	b := &Block{Block: ledger.Block{Symbol: sym, Shares: shares, Basis: d(basis), Premium: d(premium), Breakeven: be}, Price: d(spot), HasPrice: true}
	a := &Account{Name: "roth", Pool: "roth", Blocks: []*Block{b}, Ledger: &ledger.AccountLedger{},
		openOrdersKnown: true, positionsKnown: true,
		portfolio: &broker.Balance{TotalValue: broker.N(total)}}
	a.portfolio.BuyingPower = broker.N(bp)
	return &Book{AsOf: nov6, Pools: []*Pool{{Name: "roth", Accounts: []*Account{a}}}, quotes: true}
}

func want(t *testing.T, what string, got decimal.Decimal, exp string) {
	t.Helper()
	if !got.Equal(d(exp)) {
		t.Errorf("%s = %s, want %s", what, got, exp)
	}
}

func has(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// Re-covering 300 shares of AAA (basis $60, $900 of block premium, spot $54)
// with three $58 calls at $0.75.
func TestTicketRecover(t *testing.T) {
	r := loadRules(t)
	bk := ticketBook("AAA", 300, "60", "900", "54", "100000", "5000")
	tk, err := bk.Ticket(r, Proposal{Account: "roth", Kind: TradeCoveredCall, Limit: d("0.75"),
		Option: option("AAA", "call", "58.0000", "2026-12-18", "0.70", "0.79", "0.18", 5000, sessionQuote)})
	if err != nil {
		t.Fatal(err)
	}
	if !tk.Accepted {
		t.Fatalf("rejected: %v", tk.Rejections)
	}
	want(t, "qty", decimal.NewFromInt(tk.Qty), "3")
	want(t, "credit", tk.Credit, "225")
	want(t, "breakeven after", *tk.BreakevenAfter, "56.25") // (18000 − 900 − 225) ÷ 300
	want(t, "clearance", *tk.Clearance, "1.75")             // 58 − 56.25
	// 10% underwater (6 of 60): 5% − 4% × (10 − 3)/(12 − 3) = 1.8889%
	want(t, "required clearance", tk.RequiredClearance.Round(4), "0.0189")
	want(t, "call-away share gain", tk.CallAway.ShareGain, "-600") // (58 − 60) × 300
	want(t, "call-away premium", tk.CallAway.Premium, "1125")      // 900 + 225
	want(t, "call-away total", tk.CallAway.Total, "525")
	want(t, "call-away capital", tk.CallAway.Capital, "18000")
	if tk.CallAway.Days != 42 {
		t.Errorf("days = %d, want 42", tk.CallAway.Days)
	}
	want(t, "annualized", tk.CallAway.Annualized.Round(4), "0.2535") // 525 ÷ 18000 × 365 ÷ 42
	want(t, "closer", tk.Closer.Price, "0.38")                       // 0.375 rounds up to the penny
	want(t, "max loss", tk.MaxLoss, "17775")                         // 60 × 300 − 225
	if !has(tk.Warnings, "is below the 0.2–0.4 band") {
		t.Errorf("delta 0.18 should warn (the floor rule): %v", tk.Warnings)
	}
}

func TestTicketGradedClearanceNearBasis(t *testing.T) {
	r := loadRules(t)
	// Spot is near a $60 basis and the $60 strike clears the breakeven by 1.3%:
	// within 3% underwater the minimum is 5%, so it is rejected.
	bk := ticketBook("AAA", 300, "60", "0", "59.50", "100000", "0")
	tk, _ := bk.Ticket(r, Proposal{Account: "roth", Kind: TradeCoveredCall, Limit: d("0.78"),
		Option: option("AAA", "call", "60", "2026-12-18", "0.75", "0.80", "0.30", 5000, sessionQuote)})
	if tk.Accepted || !has(tk.Rejections, "graded-clearance minimum") {
		t.Fatalf("near-basis thin clearance must be rejected: %v", tk.Rejections)
	}
	want(t, "required", *tk.RequiredClearance, "0.05")
}

func TestRequiredClearanceIsLinear(t *testing.T) {
	r := loadRules(t)
	// 5% at or under 3% underwater, 1% at or over 12%, linear between.
	for depth, req := range map[string]string{"-0.05": "0.05", "0.02": "0.05", "0.03": "0.05", "0.06": "0.0367", "0.10": "0.0189", "0.12": "0.01", "0.30": "0.01"} {
		want(t, "required at "+depth, r.RequiredClearance(d(depth)).Round(4), req)
	}
}

func TestTicketCallRejections(t *testing.T) {
	r := loadRules(t)
	bk := ticketBook("AAA", 300, "60", "1500", "50", "100000", "0") // breakeven 55.00 before
	cases := map[string]struct {
		strike, bid, limit, delta string
		oi                        int64
		at                        string
		want                      string
	}{
		// breakeven 55 before; the $3 × 100 credit lowers it to 54, so $53 is below it.
		"below breakeven":  {"53", "1.00", "1.00", "0.30", 900, sessionQuote, "below the premium-adjusted breakeven"},
		"delta above band": {"56", "1.00", "1.00", "0.45", 900, sessionQuote, "is above the 0.2–0.4 band"},
		"low OI":           {"60", "1.00", "1.00", "0.30", 30, sessionQuote, "open interest 30"},
		"below the bid":    {"60", "1.10", "1.00", "0.30", 900, sessionQuote, "below the bid"},
		"off tick":         {"60", "1.00", "1.005", "0.30", 900, sessionQuote, "not on the contract's"},
		"opening bell":     {"60", "1.00", "1.00", "0.30", 900, "2026-11-06T14:35:00Z", "opening bell"},
		"after hours":      {"60", "1.00", "1.00", "0.30", 900, "2026-11-06T21:15:00Z", "after the 16:00 close"},
	}
	for name, c := range cases {
		tk, err := bk.Ticket(r, Proposal{Account: "roth", Kind: TradeCoveredCall, Limit: d(c.limit),
			Option: option("AAA", "call", c.strike, "2027-01-15", c.bid, "1.20", c.delta, c.oi, c.at)})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if tk.Accepted || !has(tk.Rejections, c.want) {
			t.Errorf("%s: want rejection containing %q, got %v", name, c.want, tk.Rejections)
		}
	}
}

func TestTicketNoFreeLots(t *testing.T) {
	r := loadRules(t)
	bk, _, _ := fixture("AAA", 300, "54", "58", "0.80", 3, "2026-12-18")
	bk.AsOf = nov6
	tk, _ := bk.Ticket(r, Proposal{Account: "t", Kind: TradeCoveredCall, Limit: d("0.75"),
		Option: option("AAA", "call", "58", "2026-12-18", "0.70", "0.79", "0.25", 5000, sessionQuote)})
	if tk.Accepted || !has(tk.Rejections, "no free lots") {
		t.Errorf("fully written block: %v", tk.Rejections)
	}
}

func TestTicketAcquisitionPut(t *testing.T) {
	r := loadRules(t)
	// A CCC $70 put at $1.10 credit needs $7,000 of collateral.
	bk := ticketBook("AAA", 100, "60", "0", "60", "35000", "10000")
	tk, _ := bk.Ticket(r, Proposal{Account: "roth", Kind: TradeAcquisitionPut, Limit: d("1.10"),
		Option: option("CCC", "put", "70", "2026-12-18", "1.05", "1.15", "-0.40", 800, sessionQuote)})
	if !tk.Accepted {
		t.Fatalf("rejected: %v", tk.Rejections)
	}
	want(t, "effective cost", *tk.EffectiveCost, "68.90") // 70 − 1.10
	want(t, "collateral", *tk.CashRequired, "7000")
	want(t, "max loss", tk.MaxLoss, "6890") // 6890 = 68.90 × 100
	if tk.Closer != nil {
		t.Error("acquisition put must not carry a closer (Rule 4a)")
	}

	bk.Pools[0].Accounts[0].portfolio.BuyingPower = broker.N("2000")
	tk, _ = bk.Ticket(r, Proposal{Account: "roth", Kind: TradeAcquisitionPut, Limit: d("1.10"),
		Option: option("CCC", "put", "70", "2026-12-18", "1.05", "1.15", "-0.40", 800, sessionQuote)})
	if tk.Accepted || !has(tk.Rejections, "Hard constraint 5") {
		t.Errorf("insufficient buying power: %v", tk.Rejections)
	}
}

func TestTicketPutBlockedOverCap(t *testing.T) {
	r := loadRules(t) // loadRules caps the {AAA, BBB} group at 50% of invested equity
	bk := ticketBook("AAA", 200, "60", "0", "50", "35000", "10000")
	tk, _ := bk.Ticket(r, Proposal{Account: "roth", Kind: TradeAcquisitionPut, Limit: d("1.50"),
		Option: option("BBB", "put", "85", "2026-12-18", "1.45", "1.55", "-0.40", 800, sessionQuote)})
	if tk.Accepted || !has(tk.Rejections, "no BBB puts here") {
		t.Errorf("group put over cap: %v", tk.Rejections)
	}
}

func TestTicketIncomePutCloser(t *testing.T) {
	r := loadRules(t)
	bk := ticketBook("AAA", 100, "60", "0", "60", "100000", "10000")
	tk, _ := bk.Ticket(r, Proposal{Account: "roth", Kind: TradeIncomePut, Limit: d("0.85"),
		Option: option("CCC", "put", "60", "2026-12-18", "0.80", "0.90", "-0.30", 800, sessionQuote)})
	if !tk.Accepted {
		t.Fatalf("rejected: %v", tk.Rejections)
	}
	want(t, "income put closer", tk.Closer.Price, "0.51") // 60% of 0.85, exactly
}

func TestCooldownBlocksNewPutsNotCalls(t *testing.T) {
	r := loadRules(t)
	bk := ticketBook("AAA", 300, "60", "900", "54", "100000", "50000")
	a := bk.Pools[0].Accounts[0]
	loss := func(sym, closed string) ledger.Leg {
		return ledger.Leg{Key: ledger.ContractKey{Symbol: sym, Type: "call", Strike: "1", Expiry: closed}, End: ledger.EndBought,
			ClosedAt: closed, OpenedAt: closed, Net: d("-5"), Kind: ledger.KindCoveredCall}
	}
	// Two losses are not enough (the rule needs three); three in a row freeze the pool.
	a.Ledger.Legs = []ledger.Leg{loss("BBB", "2026-11-03"), loss("CCC", "2026-11-04")}
	if on, _ := bk.Pools[0].Cooldown(r, nov6); on {
		t.Fatal("two losses must not freeze the pool")
	}
	a.Ledger.Legs = append(a.Ledger.Legs, loss("DDD", "2026-11-05"))
	if on, why := bk.Pools[0].Cooldown(r, nov6); !on {
		t.Fatal("three losses within ten sessions must freeze the pool")
	} else if !strings.Contains(why, "3 losses") {
		t.Errorf("reason: %s", why)
	}
	put, _ := bk.Ticket(r, Proposal{Account: "roth", Kind: TradeAcquisitionPut, Limit: d("1.10"),
		Option: option("CCC", "put", "70", "2026-12-18", "1.05", "1.15", "-0.40", 800, sessionQuote)})
	if put.Accepted || !has(put.Rejections, "cooldown") {
		t.Errorf("put during cooldown: %v", put.Rejections)
	}
	call, _ := bk.Ticket(r, Proposal{Account: "roth", Kind: TradeCoveredCall, Limit: d("0.75"),
		Option: option("AAA", "call", "58", "2026-12-18", "0.70", "0.79", "0.26", 5000, sessionQuote)})
	if has(call.Rejections, "cooldown") {
		t.Error("re-covering held shares is never blocked by the cooldown")
	}
	// Two sessions after the last loss (11/05) the freeze has run out.
	if on, _ := bk.Pools[0].Cooldown(r, time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)); on {
		t.Error("freeze should end after two sessions")
	}
	// A single loss over 2% of pool equity ($100,000) triggers on its own.
	a.Ledger.Legs = []ledger.Leg{loss("CCC", "2026-11-05")}
	a.Ledger.Legs[0].Net = d("-2100")
	if on, why := bk.Pools[0].Cooldown(r, nov6); !on || !strings.Contains(why, "over 2.0%") {
		t.Errorf("single large loss: %v %s", on, why)
	}
}

func TestSessionsBetween(t *testing.T) {
	fri := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	if n := sessionsBetween(fri, fri.AddDate(0, 0, 3)); n != 1 { // Fri → Mon
		t.Errorf("Fri→Mon = %d sessions, want 1", n)
	}
}

// Addendum A: a quote the server fetched itself is judged by the fetch time,
// not by a time anyone supplies, and a stale quote is refused.
func TestTicketFetchedQuoteWindow(t *testing.T) {
	r := loadRules(t)
	ny, _ := time.LoadLocation("America/New_York")
	session := func(h, m int) time.Time { return time.Date(2026, 11, 6, h, m, 0, 0, ny) }
	cases := []struct {
		name    string
		fetched time.Time
		quoteAt string
		reject  string // "" = no window rejection
	}{
		{"mid-session", session(11, 0), sessionQuote, ""},
		{"first ten minutes", session(9, 35), sessionQuote, "fetched before 09:30 plus the first 10 minutes"},
		{"after the close", session(16, 5), sessionQuote, "fetched after the 16:00 close"},
		{"another session", session(11, 0).AddDate(0, 0, 3), sessionQuote, "fetched in the 2026-11-09 session"},
		{"stale quote", session(11, 0), "2026-11-05T20:59:00Z", "stale"},
	}
	for _, c := range cases {
		bk := ticketBook("AAA", 300, "60", "900", "54", "100000", "5000")
		tk, err := bk.Ticket(r, Proposal{Account: "roth", Kind: TradeCoveredCall, Limit: d("0.75"), FetchedAt: c.fetched,
			Option: option("AAA", "call", "58.0000", "2026-12-18", "0.70", "0.79", "0.30", 5000, c.quoteAt)})
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case c.reject == "" && !tk.Accepted:
			t.Errorf("%s: rejected: %v", c.name, tk.Rejections)
		case c.reject != "" && !has(tk.Rejections, c.reject):
			t.Errorf("%s: want a rejection mentioning %q, got %v", c.name, c.reject, tk.Rejections)
		}
		if tk.FetchedAt != c.fetched.UTC().Format(time.RFC3339) {
			t.Errorf("%s: fetched_at %q", c.name, tk.FetchedAt)
		}
	}
}

// A put in a symbol outside every cap group, or with no caps at all, is not refused for concentration.
func TestTicketNoCapNoConcentrationRejection(t *testing.T) {
	r := loadRules(t)
	r.ConcentrationCaps = nil
	bk := ticketBook("AAA", 200, "60", "0", "50", "35000", "10000") // 100% AAA
	tk, _ := bk.Ticket(r, Proposal{Account: "roth", Kind: TradeAcquisitionPut, Limit: d("1.50"),
		Option: option("BBB", "put", "85", "2026-12-18", "1.45", "1.55", "-0.40", 800, sessionQuote)})
	if has(tk.Rejections, "puts here") {
		t.Errorf("rejected for concentration with no caps: %v", tk.Rejections)
	}
}
