package robinhood

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/branonm/options-book/internal/broker"
	"github.com/branonm/options-book/internal/broker/conformance"
)

func glob(t *testing.T, patterns ...string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, p := range patterns {
		files, err := filepath.Glob(filepath.Join(root, p))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			out[f] = b
		}
	}
	return out
}

// orderRecords is every order version in both order logs.
func orderRecords(t *testing.T) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, acct := range []string{"roth", "trad"} {
		f, err := os.Open(filepath.Join(root, "history", acct, "orders.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for n := 1; sc.Scan(); n++ {
			var e struct {
				Order json.RawMessage `json:"order"`
			}
			if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
				t.Fatal(err)
			}
			out[acct+" line "+itoa(n)] = append([]byte{}, e.Order...)
		}
		f.Close()
	}
	return out
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// The conformance suite over every Robinhood response saved in the repo.
func TestConformance(t *testing.T) {
	conformance.Run(t, Decoder{}, conformance.Samples{
		Orders:          glob(t, "snapshots/*/*/option_orders_all.json", "snapshots/*/*/option_orders_filled.json", "snapshots/*/*/orders_pull/*.json"),
		OrderRecords:    orderRecords(t),
		EquityPositions: glob(t, "snapshots/*/*/equity_positions.json"),
		OptionPositions: glob(t, "snapshots/*/*/option_positions.json"),
		Balances:        glob(t, "snapshots/*/*/portfolio.json"),
		EquityQuotes:    glob(t, "snapshots/*/equity_quotes.json"),
		OptionQuotes:    glob(t, "snapshots/*/option_quotes.json", "snapshots/*/options/*/quote.json"),
		Instruments:     glob(t, "snapshots/*/options/*/instrument.json"),
		RealizedPnL:     glob(t, "reconcile/*/*_realized_pnl.json"),
		Bars:            glob(t, "history/bars/*.json"),
	})
}

// Decoding changes no value: every order field keeps Robinhood's text.
func TestOrderTextUnchanged(t *testing.T) {
	n := 0
	for name, rec := range orderRecords(t) {
		o, err := Decoder{}.OrderRecord(name, rec)
		if err != nil {
			t.Fatal(err)
		}
		var raw map[string]any
		d := json.NewDecoder(bytes.NewReader(rec))
		d.UseNumber()
		d.Decode(&raw)
		s := func(k string) string {
			v, _ := raw[k].(string)
			return v
		}
		for field, got := range map[string]string{
			"id": o.ID, "chain_symbol": o.Symbol, "state": o.BrokerState, "quantity": o.Quantity.Text,
			"processed_quantity": o.FilledQuantity.Text, "price": o.Price.Text, "processed_premium": o.Premium.Text,
			"trade_value_multiplier": o.Multiplier.Text, "time_in_force": o.TimeInForce, "created_at": o.CreatedAt,
		} {
			if got != s(field) {
				t.Errorf("%s: %s %q, raw %q", name, field, got, s(field))
			}
		}
		legs, _ := raw["legs"].([]any)
		if len(legs) != len(o.Legs) {
			t.Fatalf("%s: %d legs, raw %d", name, len(o.Legs), len(legs))
		}
		for i, l := range legs {
			rl := l.(map[string]any)
			cl := o.Legs[i]
			for field, got := range map[string]string{"option_id": cl.InstrumentID, "side": cl.Side, "position_effect": cl.Effect,
				"expiration_date": cl.Expiry, "strike_price": cl.Strike.Text, "option_type": cl.Right} {
				if v, _ := rl[field].(string); v != got {
					t.Errorf("%s leg %d: %s %q, raw %q", name, i, field, got, v)
				}
			}
			execs, _ := rl["executions"].([]any)
			if len(execs) != len(cl.Executions) {
				t.Errorf("%s leg %d: %d executions, raw %d", name, i, len(cl.Executions), len(execs))
			}
		}
		n++
	}
	t.Logf("%d order records keep Robinhood's text", n)
}

func TestStates(t *testing.T) {
	for s, want := range map[string]broker.OrderState{
		"queued": broker.StateOpen, "unconfirmed": broker.StateOpen, "confirmed": broker.StateOpen,
		"partially_filled": broker.StateOpen, "pending_cancelled": broker.StateOpen,
		"filled": broker.StateFilled, "cancelled": broker.StateCancelled, "voided": broker.StateCancelled,
		"rejected": broker.StateRejected, "failed": broker.StateFailed, "something_new": broker.StateUnknown,
	} {
		if got := State(s); got != want {
			t.Errorf("%s → %s, want %s", s, got, want)
		}
	}
	if d, err := broker.Lookup(Name); err != nil || d.Name() != Name {
		t.Errorf("registry: %v %v", d, err)
	}
}

func TestSyncConformance(t *testing.T) {
	conformance.RunSync(t, Sync{}, Decoder{}, conformance.SyncSamples{
		Pages:   glob(t, "snapshots/*/*/option_orders_all.json", "snapshots/*/*/option_orders_filled.json", "snapshots/*/*/orders_pull/*.json"),
		Records: orderRecords(t),
	})
}

// The plan text is what `ob history plan` and the history_plan tool have
// always said, character for character.
func TestPullStepsText(t *testing.T) {
	s := Sync{}
	got := strings.Join(s.PullSteps("2026-11-12", nil), "\n")
	want := "  1. get_option_orders(created_at_gte=2026-11-12), every page\n" +
		"  2. get_option_orders(state=confirmed), plus queued and partially_filled if any"
	if got != want {
		t.Errorf("no open orders:\n%s", got)
	}
	got = strings.Join(s.PullSteps("2026-11-12", []string{"6aeb4f0a", "6af49a26"}), "\n")
	want += "\n  3. every order open in the log must appear in 1 or 2; fetch any that don't with get_option_orders(order_id=…):\n       6aeb4f0a\n       6af49a26"
	if got != want {
		t.Errorf("with open orders:\n%s", got)
	}
	if s.FullPull() != "get_option_orders (no filters, every page)" || s.Lookup() != "get_option_orders(order_id=…)" {
		t.Errorf("%q %q", s.FullPull(), s.Lookup())
	}
}

// Robinhood bumps updated_at on resting GTC orders every morning: that alone
// must not change the version, but a change to anything that matters must.
func TestHeadVersionIgnoresUpdatedAt(t *testing.T) {
	rec := func(updated, state, price string) []byte {
		return []byte(`{"id":"a","state":"` + state + `","created_at":"2026-11-01T14:00:00Z","updated_at":"` + updated + `","processed_quantity":"0","canceled_quantity":"0","quantity":"1","price":"` + price + `","time_in_force":"gtc"}`)
	}
	base, err := Sync{}.Head(rec("2026-11-02T13:30:00Z", "confirmed", "0.40"))
	if err != nil {
		t.Fatal(err)
	}
	touched, _ := Sync{}.Head(rec("2026-11-03T13:30:00Z", "confirmed", "0.40"))
	if touched.Version != base.Version || touched.UpdatedAt == base.UpdatedAt {
		t.Errorf("an updated_at touch changed the version: %q vs %q", base.Version, touched.Version)
	}
	for name, r := range map[string][]byte{"state": rec("2026-11-02T13:30:00Z", "filled", "0.40"), "price": rec("2026-11-02T13:30:00Z", "confirmed", "0.45")} {
		if h, _ := (Sync{}).Head(r); h.Version == base.Version {
			t.Errorf("a %s change didn't change the version", name)
		}
	}
	for _, bad := range []string{`{"state":"filled","created_at":"x"}`, `{"id":"a","created_at":"x"}`, `{"id":"a","state":"filled"}`, `not json`} {
		if _, err := (Sync{}).Head([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

// A page's cursor is read, not refused: the log checks a pull is complete.
func TestPage(t *testing.T) {
	p, err := Sync{}.Page("p", []byte(`{"data":{"orders":[{"id":"a"}],"next":"https://x/?cursor=c2"}}`))
	if err != nil || len(p.Records) != 1 || p.Next != "https://x/?cursor=c2" {
		t.Errorf("%+v %v", p, err)
	}
	if p, _ := (Sync{}).Page("p", []byte(`{"data":{"orders":[],"next":null}}`)); p.Next != "" {
		t.Errorf("a null cursor: %q", p.Next)
	}
	if _, err := (Sync{}).Page("p", []byte(`{`)); err == nil || !strings.HasPrefix(err.Error(), "p: ") {
		t.Errorf("a bad page: %v", err)
	}
}
