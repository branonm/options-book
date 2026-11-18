package wheel

import (
	"github.com/branonm/options-book/internal/layout"
	"path/filepath"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/branonm/options-book/internal/broker"
	"github.com/branonm/options-book/internal/ledger"
	"github.com/branonm/options-book/internal/rules"
)

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func loadRules(t *testing.T) *rules.Rules {
	t.Helper()
	r, err := rules.Load(filepath.Join("..", "..", "..", "testdata", "synthetic", filepath.FromSlash(layout.RulesFile(layout.DefaultBook))))
	if err != nil {
		t.Fatal(err)
	}
	// One invented cap group: AAA and BBB together at most 50% of invested equity.
	r.ConcentrationCaps = []rules.ConcentrationCap{{Name: "group-a", Rule: 4, Symbols: []string{"AAA", "BBB"}, MaxShare: d("0.50")}}
	return r
}

var asOf = time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)

// fixture builds a one-account book by hand: a block with one short call.
func fixture(sym string, shares int64, price, strike, fill string, qty int64, expiry string, closers ...Closer) (*Book, *Account, *ShortCall) {
	exp, _ := time.Parse("2006-01-02", expiry)
	credit := d(fill).Mul(decimal.NewFromInt(qty * 100))
	leg := ledger.Leg{Key: ledger.ContractKey{Symbol: sym, Type: "call", Strike: strike, Expiry: expiry},
		OptionID: "opt", End: ledger.EndOpen, OpenQty: qty, Qty: qty, Credit: credit, Net: credit, FillPerShare: d(fill)}
	b := &Block{Block: ledger.Block{Symbol: sym, Shares: shares, Basis: d("50")}, Price: d(price), HasPrice: true}
	c := &ShortCall{Leg: leg, Strike: d(strike), Expiry: exp, Closers: closers, block: b}
	b.Calls = []*ShortCall{c}
	a := &Account{Name: "t", Pool: "p", Blocks: []*Block{b}, Ledger: &ledger.AccountLedger{}, openOrdersKnown: true}
	bk := &Book{AsOf: asOf, Pools: []*Pool{{Name: "p", Accounts: []*Account{a}}}, quotes: true}
	return bk, a, c
}

func checks(fs []Finding) map[string]Finding {
	m := map[string]Finding{}
	for _, f := range fs {
		m[f.Check] = f
	}
	return m
}

func TestExpectedCloserRounding(t *testing.T) {
	r := loadRules(t)
	cases := []struct{ fill, want string }{
		{"1.65", "0.83"}, // 0.825 rounds up to the penny
		{"1.62", "0.81"}, // exactly 0.81
		{"0.74", "0.37"},
		{"3.12", "1.56"},
		{"0.55", "0.28"},   // 0.275 rounds up
		{"8.00", "4.00"},   // 4.00 is at/above the $3.00 cutoff: nickel ticks
		{"8.10", "4.05"},   // exactly 4.05, a nickel
		{"7.4875", "3.75"}, // 3.74375 is above the cutoff: rounds up to the next nickel
		{"5.9975", "3.00"}, // raw 2.99875 is under the cutoff, so penny tick: rounds up to 3.00
		{"0.87", "0.44"},   // 0.435 rounds UP: the fraction is a ceiling
		{"8.04", "4.05"},   // 4.02 rounds up to the next nickel
	}
	for _, c := range cases {
		_, _, sc := fixture("X", 100, "50", "60", c.fill, 1, "2026-12-18")
		if got := sc.ExpectedCloser(r).Price; !got.Equal(d(c.want)) {
			t.Errorf("fill %s: closer %s, want %s", c.fill, got, c.want)
		}
	}
}

func TestCloserChecks(t *testing.T) {
	r := loadRules(t)
	good := Closer{OrderID: "c", Qty: 3, Price: d("0.55"), Tif: "gtc", State: "confirmed"}

	bk, a, c := fixture("AAA", 300, "60", "64", "1.10", 3, "2026-12-18", good)
	if fs, _ := c.Check(r, a, bk); len(fs) != 0 {
		t.Fatalf("correct closer flagged: %v", fs)
	}

	bad := good
	bad.Price, bad.Tif, bad.Qty = d("0.60"), "gfd", 1
	bk, a, c = fixture("AAA", 300, "60", "64", "1.10", 3, "2026-12-18", bad)
	fs, _ := c.Check(r, a, bk)
	got := checks(fs)
	for _, want := range []string{"closer_price", "closer_tif", "closer_qty"} {
		if got[want].Severity != Defect {
			t.Errorf("missing defect %s in %v", want, fs)
		}
	}
	if got["closer_price"].Expected != "$0.55" || got["closer_price"].Actual != "$0.60" {
		t.Errorf("closer_price expected/actual = %s/%s", got["closer_price"].Expected, got["closer_price"].Actual)
	}

	bk, a, c = fixture("AAA", 300, "60", "64", "1.10", 3, "2026-12-18")
	if fs, _ := c.Check(r, a, bk); checks(fs)["closer_missing"].Severity != Defect {
		t.Errorf("missing closer not flagged: %v", fs)
	}
}

func TestRollTriggerNeedsITMAndDTE(t *testing.T) {
	r := loadRules(t)
	cl := Closer{Qty: 1, Price: d("0.22"), Tif: "gtc"}
	// ITM but 16 DTE → no trigger.
	bk, a, c := fixture("BBB", 100, "29.60", "28", "0.55", 1, "2026-11-20", cl)
	if fs, _ := c.Check(r, a, bk); checks(fs)["roll_trigger"].Check != "" {
		t.Error("16 DTE must not trigger Rule 3")
	}
	// Same contract with 6 DTE → trigger.
	bk.AsOf = time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	if fs, _ := c.Check(r, a, bk); checks(fs)["roll_trigger"].Severity != Alert {
		t.Error("ITM with 6 DTE must trigger Rule 3")
	}
	// OTM with 2 DTE → no trigger.
	bk, a, c = fixture("BBB", 100, "27.00", "28", "0.55", 1, "2026-11-20", cl)
	bk.AsOf = time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	if fs, _ := c.Check(r, a, bk); checks(fs)["roll_trigger"].Check != "" {
		t.Error("OTM must not trigger Rule 3")
	}
}

func TestCoverage(t *testing.T) {
	r := loadRules(t)
	cl := Closer{Qty: 1, Price: d("0.55"), Tif: "gtc"}
	bk, a, _ := fixture("AAA", 300, "60", "64", "1.10", 1, "2026-12-18", cl)
	fs, _ := a.Blocks[0].Check(r, a, bk)
	if f := checks(fs)["uncovered_shares"]; f.Severity != Alert || f.Expected != "3 contracts" {
		t.Errorf("1 contract on 300 shares: %v", fs)
	}
	bk, a, _ = fixture("AAA", 100, "60", "64", "1.10", 3, "2026-12-18", Closer{Qty: 3, Price: d("0.55"), Tif: "gtc"})
	fs, _ = a.Blocks[0].Check(r, a, bk)
	if checks(fs)["calls_exceed_shares"].Severity != Defect {
		t.Errorf("3 contracts on 100 shares: %v", fs)
	}
	// An odd lot: 10 shares, no calls → nothing to flag.
	bk, a, _ = fixture("CCC", 10, "377.50", "400", "1.00", 1, "2026-12-18")
	a.Blocks[0].Calls = nil
	if fs, _ := a.Blocks[0].Check(r, a, bk); len(fs) != 0 {
		t.Errorf("odd lot flagged: %v", fs)
	}
}

func TestAcquisitionPutMustNotHaveCloser(t *testing.T) {
	r := loadRules(t)
	a := &Account{Name: "t", openOrdersKnown: true}
	p := &ShortPut{Leg: ledger.Leg{Key: ledger.ContractKey{Symbol: "BBB", Type: "put", Strike: "70", Expiry: "2026-12-18"}}}
	if fs, _ := p.Check(r, a); len(fs) != 0 {
		t.Errorf("put with no closer flagged: %v", fs)
	}
	p.Closers = []Closer{{Qty: 1, Price: d("0.50"), Tif: "gtc"}}
	if fs, _ := p.Check(r, a); checks(fs)["acquisition_put_closer"].Severity != Defect {
		t.Errorf("closer on acquisition put not flagged: %v", fs)
	}
}

func TestPositionReconciliation(t *testing.T) {
	r := loadRules(t)
	cl := Closer{Qty: 3, Price: d("0.55"), Tif: "gtc"}
	bk, a, c := fixture("AAA", 300, "60", "64", "1.10", 3, "2026-12-18", cl)
	a.positionsKnown = true
	c.Position = &broker.OptionPosition{Side: "short", Quantity: broker.N("3.0000"), AveragePrice: broker.N("-110.0000"), PendingBuyQuantity: broker.N("3.0000")}
	if fs, _ := c.Check(r, a, bk); len(fs) != 0 {
		t.Fatalf("matching position flagged: %v", fs)
	}
	c.Position = &broker.OptionPosition{Side: "short", Quantity: broker.N("2.0000"), AveragePrice: broker.N("-100.0000"), PendingBuyQuantity: broker.N("0.0000")}
	got := checks(func() []Finding { fs, _ := c.Check(r, a, bk); return fs }())
	for _, w := range []string{"position_qty", "position_credit", "pending_buy_lag"} {
		if got[w].Check == "" {
			t.Errorf("missing %s", w)
		}
	}
	c.Position = nil
	if fs, _ := c.Check(r, a, bk); checks(fs)["position_missing"].Severity != Defect {
		t.Error("missing position not flagged")
	}
}

// A source that does not report pending buys (Alpaca) leaves the field empty;
// that is unknown, not zero, so no pending_buy_lag and no unreadable position.
func TestPendingBuyNotReported(t *testing.T) {
	r := loadRules(t)
	cl := Closer{Qty: 3, Price: d("0.55"), Tif: "gtc"}
	bk, a, c := fixture("AAA", 300, "60", "64", "1.10", 3, "2026-12-18", cl)
	a.positionsKnown = true
	c.Position = &broker.OptionPosition{Side: "short", Quantity: broker.N("3.0000"), AveragePrice: broker.N("-110.0000")}
	if fs, _ := c.Check(r, a, bk); len(fs) != 0 {
		t.Fatalf("position with no pending-buy field flagged: %v", fs)
	}
}

func TestConcentrationCap(t *testing.T) {
	r := loadRules(t)
	bk, _, _ := fixture("AAA", 200, "50", "60", "1.85", 2, "2027-03-19")
	a := bk.Pools[0].Accounts[0]
	a.Blocks = append(a.Blocks, &Block{Block: ledger.Block{Symbol: "CCC", Shares: 100}, Price: d("40"), HasPrice: true})
	// AAA 10,000 of 14,000 = 71.4% → over the cap.
	if fs, _ := bk.Pools[0].Check(r, bk); len(fs) != 1 || fs[0].Actual != "71.4%" {
		t.Errorf("over cap: %v", fs)
	}
	a.Blocks[1].Shares = 1000 // CCC 40,000 → AAA 20.0%
	if fs, _ := bk.Pools[0].Check(r, bk); len(fs) != 0 {
		t.Errorf("under cap flagged: %v", fs)
	}
}

func TestOpenPremium(t *testing.T) {
	r := loadRules(t)
	// AAA 3 @ $1.10 ($330) with a $0.60 live closer; Rule 10 closer is $0.55.
	bk, a, _ := fixture("AAA", 300, "60", "64", "1.10", 3, "2026-12-18", Closer{Qty: 3, Price: d("0.60"), Tif: "gtc"})
	a.Ledger.Realized = d("100")
	op := bk.OpenPremium(r)
	for name, got := range map[string]decimal.Decimal{"gross": op.Gross, "live": op.NetAtLive, "rule10": op.NetAtRule10, "realized": op.RealizedSoFar} {
		want := map[string]string{"gross": "330", "live": "150", "rule10": "165", "realized": "100"}[name]
		if !got.Equal(d(want)) {
			t.Errorf("%s = %s, want %s", name, got, want)
		}
	}
	a.Blocks[0].Calls[0].Closers = nil
	if op := bk.OpenPremium(r); op.MissingLive != 1 || !op.NetAtLive.IsZero() {
		t.Errorf("missing closer: %+v", op)
	}
}

// With no concentration caps there is nothing to enforce, however concentrated the book.
func TestNoCapsNoFinding(t *testing.T) {
	r := loadRules(t)
	r.ConcentrationCaps = nil
	bk, _, _ := fixture("AAA", 200, "50", "60", "1.85", 2, "2027-03-19")
	rep := bk.Check(r)
	for _, f := range append(rep.Defects, rep.Alerts...) {
		if f.Check == "concentration_cap" {
			t.Errorf("concentration finding with no caps: %+v", f)
		}
	}
}

// Several caps are checked independently, each reported under its own name.
func TestSeveralCaps(t *testing.T) {
	r := loadRules(t)
	r.ConcentrationCaps = append(r.ConcentrationCaps,
		rules.ConcentrationCap{Name: "aaa-alone", Symbols: []string{"AAA"}, MaxShare: d("0.10")},
		rules.ConcentrationCap{Name: "ccc-alone", Symbols: []string{"CCC"}, MaxShare: d("0.90")})
	bk, _, _ := fixture("AAA", 200, "50", "60", "1.85", 2, "2027-03-19")
	a := bk.Pools[0].Accounts[0]
	a.Blocks = append(a.Blocks, &Block{Block: ledger.Block{Symbol: "CCC", Shares: 100}, Price: d("40"), HasPrice: true})
	fs, passed := bk.Pools[0].Check(r, bk)
	// AAA is 71.4%: over "group-a" (50%) and "aaa-alone" (10%); CCC 28.6% is under its 90%.
	if len(fs) != 2 || passed != 1 {
		t.Fatalf("findings %v, passed %d", fs, passed)
	}
	if fs[0].Subject != "pool "+bk.Pools[0].Name+" (group-a)" || fs[1].Subject != "pool "+bk.Pools[0].Name+" (aaa-alone)" {
		t.Errorf("subjects: %q, %q", fs[0].Subject, fs[1].Subject)
	}
	if fs[0].Rule != "Rule 4" || fs[1].Rule != "" {
		t.Errorf("rules: %q, %q (a cap with no rule number cites none)", fs[0].Rule, fs[1].Rule)
	}
}
