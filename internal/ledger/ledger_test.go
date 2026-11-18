package ledger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/branonm/options-book/internal/broker"
)

// Every order, price and position below is invented, and every expectation is
// worked out by hand in a comment. The campaign starts 2026-01-05.
const campaign = "2026-01-05"

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// order builds a synthetic single-leg filled order. execs are "price:qty".
func order(id, sym, side, effect, typ, strike, exp, ts string, execs ...string) broker.Order {
	var es []broker.Execution
	total, qty := decimal.Zero, decimal.Zero
	for _, e := range execs {
		p, q, _ := strings.Cut(e, ":")
		es = append(es, broker.Execution{ID: id + e, Price: broker.N(p), Quantity: broker.N(q), Time: ts})
		total = total.Add(d(p).Mul(d(q)).Mul(d("100")))
		qty = qty.Add(d(q))
	}
	return broker.Order{
		ID: id, Symbol: sym, State: broker.StateFilled, BrokerState: "filled", Multiplier: broker.N("100.0000"),
		Premium: broker.N(total.String()), FilledQuantity: broker.N(qty.StringFixed(5)), TimeInForce: "gtc",
		CreatedAt: ts,
		Legs: []broker.Leg{{InstrumentID: sym + strike + exp + typ, Side: side, Effect: effect,
			Right: typ, Strike: broker.N(strike), Expiry: exp, Executions: es}},
	}
}

func eq(sym, qty, avg string) broker.EquityPosition {
	return broker.EquityPosition{Symbol: sym, Quantity: broker.N(qty), AverageCost: broker.N(avg), Side: "long"}
}

func build(t *testing.T, orders []broker.Order, equity []broker.EquityPosition) *AccountLedger {
	t.Helper()
	l, err := Build(Input{Account: "t", AsOf: "2026-11-04", CampaignStart: campaign, Orders: orders, Equity: equity})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func want(t *testing.T, what string, got decimal.Decimal, exp string) {
	t.Helper()
	if !got.Equal(d(exp)) {
		t.Errorf("%s = %s, want %s", what, got, exp)
	}
}

func TestBreakevenIncludesOpenLegAndBlendsFills(t *testing.T) {
	// AAA: a $68 call sold at 1.00 and bought back at 0.40 (3 contracts, net $180),
	// then a $64 call opened in two executions, 1 at 1.00 and 2 at 1.30 = $360.
	l := build(t, []broker.Order{
		order("a", "AAA", "sell", "open", "call", "68.0000", "2026-09-18", "2026-08-19T18:06:33Z", "1.00:3"),
		order("b", "AAA", "buy", "close", "call", "68.0000", "2026-09-18", "2026-08-20T13:30:01Z", "0.40:3"),
		order("c", "AAA", "sell", "open", "call", "64.0000", "2026-12-18", "2026-10-30T15:09:33Z", "1.00:1", "1.30:2"),
	}, []broker.EquityPosition{eq("AAA", "300.000000", "60.000000")})

	want(t, "realized", l.Realized, "180") // 300 − 120
	if l.RealizedLegs != 1 || l.ProfitableLegs != 1 {
		t.Fatalf("legs %d profitable %d, want 1/1", l.RealizedLegs, l.ProfitableLegs)
	}
	want(t, "open credit", l.OpenCredit, "360")
	for _, lg := range l.Legs {
		if lg.End == EndOpen {
			want(t, "blended fill", lg.FillPerShare, "1.20") // 360 ÷ 300
		}
	}
	b := l.Blocks[0]
	want(t, "block premium", b.Premium, "540") // 180 + 360
	want(t, "breakeven", b.Breakeven, "58.20") // (300 × 60 − 540) ÷ 300
}

func TestAcquisitionPutGoesToBasisNotPremium(t *testing.T) {
	l := build(t, []broker.Order{
		order("p", "BBB", "sell", "open", "put", "70.0000", "2026-10-16", "2026-09-28T15:00:00Z", "1.10:1"),
	}, []broker.EquityPosition{eq("BBB", "100", "68.90")})
	want(t, "basis credit", l.BasisCredit, "110")
	want(t, "realized", l.Realized, "0")
	want(t, "block premium", l.Blocks[0].Premium, "0")
	if l.RealizedLegs != 0 {
		t.Fatalf("acquisition put counted as a realized leg")
	}
}

func TestShortPutClosedEarlyIsPremium(t *testing.T) {
	l := build(t, []broker.Order{
		order("p", "CCC", "sell", "open", "put", "88.0000", "2026-09-18", "2026-08-20T15:00:00Z", "1.20:2"),
		order("q", "CCC", "buy", "close", "put", "88.0000", "2026-09-18", "2026-08-25T15:00:00Z", "0.45:2"),
	}, []broker.EquityPosition{eq("CCC", "300", "90.00")})
	want(t, "realized", l.Realized, "150") // 240 − 90
	want(t, "basis credit", l.BasisCredit, "0")
}

func TestExpiredCallIsRealizedAtFullCredit(t *testing.T) {
	l := build(t, []broker.Order{
		order("c", "DDD", "sell", "open", "call", "64.0000", "2026-10-23", "2026-09-30T15:00:00Z", "0.80:3"),
	}, nil)
	want(t, "realized", l.Realized, "240")
	if l.Legs[0].End != EndExpired {
		t.Fatalf("end = %s, want %s", l.Legs[0].End, EndExpired)
	}
}

func TestRoundTripLossIsCounted(t *testing.T) {
	// Sold at 0.50, bought back minutes later at 0.50 and 0.55.
	l := build(t, []broker.Order{
		order("o", "EEE", "sell", "open", "call", "59.0000", "2026-09-18", "2026-08-19T18:09:12Z", "0.50:3"),
		order("c", "EEE", "buy", "close", "call", "59.0000", "2026-09-18", "2026-08-19T18:14:47Z", "0.50:2", "0.55:1"),
	}, nil)
	want(t, "realized", l.Realized, "-5") // 150 − (100 + 55)
	if l.ProfitableLegs != 0 {
		t.Fatalf("losing leg counted as profitable")
	}
}

func TestPreCampaignExcluded(t *testing.T) {
	l := build(t, []broker.Order{
		order("o", "GGG", "sell", "open", "call", "15.0000", "2025-07-18", "2025-06-25T15:00:00Z", "2.50:1"),
	}, nil)
	want(t, "realized", l.Realized, "0")
	if l.PreCampaign != 1 {
		t.Fatalf("pre-campaign count = %d", l.PreCampaign)
	}
}

func TestRejectsInconsistentOrders(t *testing.T) {
	run := func(orders ...broker.Order) error {
		_, err := Build(Input{AsOf: "2026-11-04", CampaignStart: campaign, Orders: orders})
		return err
	}
	bad := order("x", "FFF", "sell", "open", "call", "60.0000", "2027-03-19", "2026-10-26T15:00:00Z", "1.62:3")
	bad.Premium = broker.N("500") // the executions total $486
	if run(bad) == nil {
		t.Error("expected error when executions disagree with processed_premium")
	}
	subcent := order("s", "FFF", "sell", "open", "call", "60.0000", "2027-03-19", "2026-10-26T15:00:00Z", "1.62345:1")
	if run(subcent) == nil {
		t.Error("expected error on a sub-cent execution total")
	}
	if run(
		order("o", "FFF", "sell", "open", "call", "60.0000", "2027-03-19", "2026-10-26T15:00:00Z", "1.62:1"),
		order("c", "FFF", "buy", "close", "call", "60.0000", "2027-03-19", "2026-10-27T15:00:00Z", "0.65:2"),
	) == nil {
		t.Error("expected error when a close buys more contracts than are open")
	}
	if run(order("l", "HHH", "buy", "open", "call", "600.0000", "2026-12-18", "2026-10-26T15:00:00Z", "1.00:1")) == nil {
		t.Error("expected error on a buy-to-open leg")
	}
}

func TestStrikeKeyNormalized(t *testing.T) {
	// "68.0000" and "68" must be the same contract.
	l := build(t, []broker.Order{
		order("a", "AAA", "sell", "open", "call", "68.0000", "2026-09-18", "2026-08-19T18:06:33Z", "1.00:1"),
		order("b", "AAA", "buy", "close", "call", "68", "2026-09-18", "2026-08-20T13:30:01Z", "0.50:1"),
	}, nil)
	if len(l.Legs) != 1 || l.Legs[0].End != EndBought {
		t.Fatalf("strike forms not matched: %d legs", len(l.Legs))
	}
}

func expMap(t *testing.T, recs ...[3]string) map[string]Expiration { // contract key string parts via ContractKey, close
	t.Helper()
	m := map[string]Expiration{}
	for _, r := range recs {
		var k ContractKey
		switch r[0] {
		case "BBB70p":
			k = ContractKey{"BBB", "put", "70", "2026-10-16"}
		case "BBB74c":
			k = ContractKey{"BBB", "call", "74", "2026-11-20"}
		case "DDD64c":
			k = ContractKey{"DDD", "call", "64", "2026-10-23"}
		case "DDD66c":
			k = ContractKey{"DDD", "call", "66", "2026-11-20"}
		case "AAA60p":
			k = ContractKey{"AAA", "put", "60", "2026-10-16"}
		}
		e, err := NewExpiration(k, d(r[1]), "test")
		if err != nil {
			t.Fatal(err)
		}
		m[k.String()] = e
	}
	return m
}

func buildX(t *testing.T, asOf string, orders []broker.Order, equity []broker.EquityPosition, exps map[string]Expiration) *AccountLedger {
	t.Helper()
	l, err := Build(Input{Account: "t", AsOf: asOf, CampaignStart: campaign, Orders: orders, Equity: equity, Expirations: exps})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// An acquisition put stays basis credit after its shares are called away.
func TestAcquisitionPutSurvivesCallAway(t *testing.T) {
	orders := []broker.Order{
		order("p", "BBB", "sell", "open", "put", "70.0000", "2026-10-16", "2026-09-28T15:00:00Z", "1.10:1"),
		order("c", "BBB", "sell", "open", "call", "74.0000", "2026-11-20", "2026-10-20T15:00:00Z", "0.90:1"),
	}
	exps := expMap(t, [3]string{"BBB70p", "69.50"}, [3]string{"BBB74c", "75.00"})
	// After 11/20 the shares are gone: no equity position.
	l := buildX(t, "2026-11-23", orders, nil, exps)
	want(t, "basis credit", l.BasisCredit, "110")
	want(t, "realized", l.Realized, "90") // only the call's premium
	want(t, "assigned call premium", l.AssignedCallPremium, "90")
	if len(l.Warnings) != 0 {
		t.Errorf("warnings: %v", l.Warnings)
	}
	// Without the recorded outcome, the old behaviour is flagged, not silent.
	l = buildX(t, "2026-11-23", orders, nil, nil)
	if len(l.Warnings) != 2 {
		t.Errorf("unresolved expirations must warn: %v", l.Warnings)
	}
}

// An out-of-the-money put that expires is realized premium, shares or not.
func TestWorthlessPutIsPremium(t *testing.T) {
	orders := []broker.Order{order("p", "AAA", "sell", "open", "put", "60.0000", "2026-10-16", "2026-09-28T15:00:00Z", "0.50:1")}
	l := buildX(t, "2026-11-04", orders, []broker.EquityPosition{eq("AAA", "100", "66.00")}, expMap(t, [3]string{"AAA60p", "62.00"}))
	want(t, "realized", l.Realized, "50")
	want(t, "basis credit", l.BasisCredit, "0")
	if l.Legs[0].End != EndWorthless || l.Legs[0].Kind != KindShortPut {
		t.Errorf("leg: %s %s", l.Legs[0].End, l.Legs[0].Kind)
	}
}

// A re-bought symbol starts a fresh block after a full call-away.
func TestRebuyAfterCallAwayStartsFreshBlock(t *testing.T) {
	orders := []broker.Order{
		order("a", "DDD", "sell", "open", "call", "67.0000", "2026-10-16", "2026-09-16T15:00:00Z", "1.20:3"),
		order("b", "DDD", "buy", "close", "call", "67.0000", "2026-10-16", "2026-09-16T18:00:00Z", "0.60:3"),
		order("c", "DDD", "sell", "open", "call", "64.0000", "2026-10-23", "2026-09-30T15:00:00Z", "0.80:3"),
		// Re-bought after the 10/23 call-away, then covered again.
		order("d", "DDD", "sell", "open", "call", "66.0000", "2026-11-20", "2026-11-02T15:00:00Z", "1.00:1"),
	}
	exps := expMap(t, [3]string{"DDD64c", "69.50"})
	l := buildX(t, "2026-11-04", orders, []broker.EquityPosition{eq("DDD", "100", "65.00")}, exps)
	b := l.Blocks[0]
	want(t, "fresh block premium", b.Premium, "100")
	if b.Since != "2026-10-24" || b.Legs != 1 {
		t.Errorf("block since %s with %d legs, want 2026-10-24 and 1", b.Since, b.Legs)
	}
	want(t, "breakeven", b.Breakeven, "64.00") // 65.00 − 1.00
	// The old block's premium is still realized; it just isn't this block's.
	want(t, "realized", l.Realized, "420") // 180 + 240
}

func TestPartialCallAwayWarnsAndKeepsPremium(t *testing.T) {
	orders := []broker.Order{
		order("c", "DDD", "sell", "open", "call", "64.0000", "2026-10-23", "2026-09-30T15:00:00Z", "0.80:1"),
		order("e", "DDD", "sell", "open", "call", "66.0000", "2026-11-20", "2026-10-01T15:00:00Z", "1.00:1"),
	}
	l := buildX(t, "2026-11-04", orders, []broker.EquityPosition{eq("DDD", "100", "65.00")}, expMap(t, [3]string{"DDD64c", "69.50"}))
	if len(l.Warnings) != 1 || !strings.Contains(l.Warnings[0], "partial call-away") {
		t.Fatalf("warnings: %v", l.Warnings)
	}
	want(t, "block premium not reset", l.Blocks[0].Premium, "180") // 80 + 100
}

func TestLoadExpirationsRejectsBadRecords(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "e.json")
		os.WriteFile(p, []byte(body), 0o644)
		return p
	}
	good := `{"contracts":[{"contract":"DDD $64c 2026-10-23","symbol":"DDD","type":"call","strike":"64","expiry":"2026-10-23","underlying_close":"69.50","outcome":"assigned","source":"t"}]}`
	if m, err := LoadExpirations(write(good)); err != nil || len(m) != 1 {
		t.Fatalf("good file: %v", err)
	}
	for name, body := range map[string]string{
		"wrong outcome": strings.Replace(good, `"assigned"`, `"expired_worthless"`, 1),
		"key mismatch":  strings.Replace(good, `"DDD $64c 2026-10-23"`, `"DDD $65c 2026-10-23"`, 1),
		"duplicate":     strings.Replace(good, `}]}`, `},`+good[14:len(good)-2]+`]}`, 1),
	} {
		if _, err := LoadExpirations(write(body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if m, err := LoadExpirations(filepath.Join(dir, "missing.json")); err != nil || len(m) != 0 {
		t.Errorf("missing file should be empty: %v", err)
	}
}
