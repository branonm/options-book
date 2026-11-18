package alpaca

import (
	"encoding/json"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/branonm/options-book/internal/broker"
	"github.com/branonm/options-book/internal/broker/conformance"
)

func sample(t *testing.T, name string) map[string][]byte {
	t.Helper()
	p := filepath.Join("testdata", name)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return map[string][]byte{p: b}
}

func records(t *testing.T) map[string][]byte {
	t.Helper()
	page, err := Sync{}.Page("orders", sample(t, "orders.json")["testdata/orders.json"])
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for i, r := range page.Records {
		out["record "+string(rune('a'+i))] = r
	}
	return out
}

// The conformance suite over the synthetic samples.
func TestConformance(t *testing.T) {
	conformance.Run(t, Decoder{}, conformance.Samples{
		Orders:          sample(t, "orders.json"),
		OrderRecords:    records(t),
		EquityPositions: sample(t, "positions.json"),
		OptionPositions: sample(t, "positions.json"),
		Balances:        sample(t, "account.json"),
		EquityQuotes:    sample(t, "equity_trades.json"),
		OptionQuotes:    sample(t, "option_snapshots.json"),
		Instruments:     sample(t, "contracts.json"),
		Bars:            sample(t, "bars.json"),
	})
}

func TestSyncConformance(t *testing.T) {
	conformance.RunSync(t, Sync{}, Decoder{}, conformance.SyncSamples{Pages: sample(t, "orders.json"), Records: records(t)})
}

func TestOrderMapping(t *testing.T) {
	os, err := Decoder{}.Orders("orders", sample(t, "orders.json")["testdata/orders.json"])
	if err != nil {
		t.Fatal(err)
	}
	if len(os) != 5 {
		t.Fatalf("5 option orders (the equity order is skipped), got %d", len(os))
	}
	f := os[0]
	if f.State != broker.StateFilled || f.Symbol != "AAA" || f.TimeInForce != "gfd" || f.Premium.Text != "690" || f.Multiplier.Text != "100" {
		t.Errorf("filled order: %+v", f)
	}
	l := f.Legs[0]
	if l.Side != broker.Sell || l.Effect != broker.Open || l.Right != broker.Call || l.Strike.Text != "55" || l.Expiry != "2027-06-17" ||
		l.InstrumentID != "AAA270617C00055000" || len(l.Executions) != 1 || l.Executions[0].Time != "2026-11-10T15:31:02.880Z" {
		t.Errorf("filled leg: %+v", l)
	}
	if !os[1].IsOpen() || os[1].TimeInForce != "gtc" || os[1].Legs[0].Effect != broker.Close {
		t.Errorf("open closer: %+v", os[1])
	}
	if os[2].State != broker.StateCancelled || os[4].State != broker.StateCancelled {
		t.Error("expired and replaced orders are cancelled")
	}
}

func TestPositionMapping(t *testing.T) {
	body := sample(t, "positions.json")["testdata/positions.json"]
	eq, _ := Decoder{}.EquityPositions("p", body)
	op, err := Decoder{}.OptionPositions("p", body)
	if err != nil || len(eq) != 1 || len(op) != 1 {
		t.Fatalf("got %d equity, %d option, %v", len(eq), len(op), err)
	}
	p := op[0]
	if p.Side != broker.Short || p.Quantity.Text != "3" || p.AveragePrice.Text != "-230" || p.Symbol != "AAA" || p.Expiry != "2027-06-17" {
		t.Errorf("short call: %+v", p)
	}
}

func TestQuoteMapping(t *testing.T) {
	qs, err := Decoder{}.OptionQuotes("q", sample(t, "option_snapshots.json")["testdata/option_snapshots.json"])
	if err != nil || len(qs) != 1 {
		t.Fatal(qs, err)
	}
	q := qs[0]
	if q.Bid.Text != "2.17" || q.Ask.Text != "2.27" || q.Mark.Text != "2.22" || q.Delta.Text != "0.297" || q.Volume != 318 {
		t.Errorf("quote: %+v", q)
	}
}

func TestRefusals(t *testing.T) {
	if _, err := (Decoder{}).RealizedPnL("x", []byte(`{}`)); err == nil {
		t.Error("Alpaca has no realized P&L; the decoder must say so")
	}
	if _, err := (Decoder{}).Instruments("x", []byte(`{"option_contracts":[],"next_page_token":"abc"}`)); err == nil {
		t.Error("a contracts pull with a next_page_token is incomplete")
	}
	if _, err := (Decoder{}).OrderRecord("x", []byte(`{"id":"1","created_at":"t","status":"filled","asset_class":"us_option","symbol":"AAA270617C00055000","qty":"1","side":"sell"}`)); err == nil {
		t.Error("an option order without a position_intent must be refused, not guessed")
	}
	if State("done_for_day") != broker.StateUnknown {
		t.Error("an unknown status stays unknown")
	}
}

func TestFullPageIsIncomplete(t *testing.T) {
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < PageLimit; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"id":"x","created_at":"2026-11-10T15:31:02Z","status":"new"}`)
	}
	b.WriteString("]")
	p, err := Sync{}.Page("p", []byte(b.String()))
	if err != nil || p.Next == "" {
		t.Errorf("a full page must report a successor: %q %v", p.Next, err)
	}
}

// The adapter is pure: it imports neither Robinhood's shapes nor the live
// paper client.
func TestImportsNothingLive(t *testing.T) {
	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") {
			return err
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			v := imp.Path.Value
			if strings.Contains(v, "options-book/internal/rh") || v == `"github.com/branonm/options-book/internal/alpaca"` || strings.Contains(v, "alpacahq") || v == `"net/http"` {
				t.Errorf("%s imports %s", p, v)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestEquityOrdersStayOutOfTheOptionLog(t *testing.T) {
	body := sample(t, "orders.json")["testdata/orders.json"]
	page, err := Sync{}.Page("p", body)
	if err != nil || len(page.Records) != 5 {
		t.Fatalf("page keeps the 5 option orders, got %d (%v)", len(page.Records), err)
	}
	var eq []map[string]any
	_ = json.Unmarshal(body, &eq)
	rec, _ := json.Marshal(eq[5])
	if _, err := (Decoder{}).OrderRecord("x", rec); err == nil {
		t.Error("an equity order record must be refused by the option decoder")
	}
}

// Open interest lives on the contract; Instruments carries it.
func TestInstrumentOpenInterest(t *testing.T) {
	is, err := (Decoder{}).Instruments("contracts.json", sample(t, "contracts.json")["testdata/contracts.json"])
	if err != nil || len(is) != 1 {
		t.Fatal(is, err)
	}
	if is[0].OpenInterest != 14460 {
		t.Errorf("open interest = %d, want 14460", is[0].OpenInterest)
	}
}
