// Package conformance is the suite every broker adapter's decoder must pass
// on its own sample data (PLAN.md item 6). It checks the canonical shape, not
// any one broker's figures: known states, canonical sides, effects and rights,
// numbers that parse, unique order ids.
package conformance

import (
	"strings"
	"testing"

	"github.com/branonm/options-book/internal/broker"
)

// Samples are saved responses, by kind: file name → body.
type Samples struct {
	Orders          map[string][]byte
	OrderRecords    map[string][]byte
	EquityPositions map[string][]byte
	OptionPositions map[string][]byte
	Balances        map[string][]byte
	EquityQuotes    map[string][]byte
	OptionQuotes    map[string][]byte
	Instruments     map[string][]byte
	RealizedPnL     map[string][]byte
	Bars            map[string][]byte
}

func num(t *testing.T, where string, n broker.Num, optional bool) {
	t.Helper()
	if n.Text == "" && optional {
		return
	}
	if _, err := n.Dec(); err != nil {
		t.Errorf("%s: %v", where, err)
	}
}

func checkOrder(t *testing.T, where string, o broker.Order) {
	t.Helper()
	if o.ID == "" || o.CreatedAt == "" || o.BrokerState == "" {
		t.Errorf("%s: order missing id, created_at or the broker's state", where)
	}
	if o.State == broker.StateUnknown || o.State == "" {
		t.Errorf("%s: order %s has state %q, which the adapter doesn't map", where, o.ID, o.BrokerState)
	}
	num(t, where+" quantity", o.Quantity, false)
	if len(o.Legs) == 0 {
		t.Errorf("%s: order %s has no legs", where, o.ID)
	}
	for _, l := range o.Legs {
		if (l.Side != broker.Buy && l.Side != broker.Sell) || (l.Effect != broker.Open && l.Effect != broker.Close) || (l.Right != broker.Call && l.Right != broker.Put) {
			t.Errorf("%s: order %s leg %s/%s/%s is not canonical", where, o.ID, l.Side, l.Effect, l.Right)
		}
		num(t, where+" strike", l.Strike, false)
		if o.State == broker.StateFilled && len(l.Executions) == 0 {
			t.Errorf("%s: filled order %s has a leg with no executions", where, o.ID)
		}
		for _, e := range l.Executions {
			num(t, where+" execution price", e.Price, false)
			num(t, where+" execution quantity", e.Quantity, false)
		}
	}
}

// Run decodes every sample and checks the canonical shape. It reports how
// many records it checked, so a suite that silently decodes nothing fails.
func Run(t *testing.T, d broker.Decoder, s Samples) {
	n := 0
	for name, b := range s.Orders {
		os, err := d.Orders(name, b)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		seen := map[string]bool{}
		for _, o := range os {
			if seen[o.ID] {
				t.Errorf("%s: order %s twice", name, o.ID)
			}
			seen[o.ID] = true
			checkOrder(t, name, o)
			n++
		}
	}
	for name, b := range s.OrderRecords {
		o, err := d.OrderRecord(name, b)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		checkOrder(t, name, o)
		n++
	}
	for name, b := range s.EquityPositions {
		ps, err := d.EquityPositions(name, b)
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
		for _, p := range ps {
			if p.Symbol == "" {
				t.Errorf("%s: position without a symbol", name)
			}
			num(t, name+" "+p.Symbol+" quantity", p.Quantity, false)
			num(t, name+" "+p.Symbol+" average cost", p.AverageCost, false)
			n++
		}
	}
	for name, b := range s.OptionPositions {
		ps, err := d.OptionPositions(name, b)
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
		for _, p := range ps {
			if p.InstrumentID == "" || (p.Side != broker.Long && p.Side != broker.Short) {
				t.Errorf("%s: option position %q side %q", name, p.InstrumentID, p.Side)
			}
			num(t, name+" quantity", p.Quantity, false)
			num(t, name+" average price", p.AveragePrice, false)
			n++
		}
	}
	for name, b := range s.Balances {
		bal, err := d.Balance(name, b)
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
		num(t, name+" total value", bal.TotalValue, false)
		n++
	}
	for name, b := range s.EquityQuotes {
		qs, err := d.EquityQuotes(name, b)
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
		for _, q := range qs {
			num(t, name+" "+q.Symbol+" last", q.Last, false)
			n++
		}
	}
	for name, b := range s.OptionQuotes {
		qs, err := d.OptionQuotes(name, b)
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
		for _, q := range qs {
			if q.InstrumentID == "" {
				t.Errorf("%s: quote without an instrument", name)
			}
			num(t, name+" mark", q.Mark, true)
			n++
		}
	}
	for name, b := range s.Instruments {
		is, err := d.Instruments(name, b)
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
		for _, in := range is {
			if in.ID == "" || (in.Right != broker.Call && in.Right != broker.Put) {
				t.Errorf("%s: instrument %q right %q", name, in.ID, in.Right)
			}
			num(t, name+" strike", in.Strike, false)
			n++
		}
	}
	for name, b := range s.RealizedPnL {
		p, err := d.RealizedPnL(name, b)
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
		num(t, name+" total", p.Total, false)
		n++
	}
	for name, b := range s.Bars {
		bs, err := d.Bars(name, b)
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
		for _, s := range bs {
			for _, bar := range s.Bars {
				num(t, name+" "+s.Symbol+" close", bar.Close, false)
				n++
			}
		}
	}
	if n == 0 {
		t.Fatal("conformance: no records decoded")
	}
	t.Logf("%s: %d records conform", d.Name(), n)
}

// SyncSamples are saved order data for an adapter's OrderSync: list pages
// (as an order pull saves them) and single order records (as an order log
// holds them).
type SyncSamples struct {
	Pages   map[string][]byte
	Records map[string][]byte
}

// RunSync checks an OrderSync against its own broker's Decoder on the same
// samples: every record has a usable head; the sync's idea of a final state
// agrees with the canonical state the decoder gives the same record; a page
// yields heads for each of its records; the plan text is non-empty and names
// every order it must account for.
func RunSync(t *testing.T, s broker.OrderSync, d broker.Decoder, smp SyncSamples) {
	t.Helper()
	n := 0
	check := func(where string, rec []byte) {
		t.Helper()
		h, err := s.Head(rec)
		if err != nil {
			t.Errorf("%s: %v", where, err)
			return
		}
		if h.ID == "" || h.State == "" || h.CreatedAt == "" || h.Version == "" {
			t.Errorf("%s: head %+v is incomplete", where, h)
		}
		o, err := d.OrderRecord(where, rec)
		if err != nil {
			t.Errorf("%s: %v", where, err)
			return
		}
		if o.ID != h.ID || o.BrokerState != h.State {
			t.Errorf("%s: the decoder and the sync disagree on the id or state (%s/%s vs %s/%s)", where, o.ID, o.BrokerState, h.ID, h.State)
		}
		final := o.State == broker.StateFilled || o.State == broker.StateCancelled || o.State == broker.StateRejected || o.State == broker.StateFailed
		if s.Terminal(h.State) != final {
			t.Errorf("%s: state %q is terminal=%v by the sync but canonical %s", where, h.State, s.Terminal(h.State), o.State)
		}
		n++
	}
	for name, rec := range smp.Records {
		check(name, rec)
	}
	for name, body := range smp.Pages {
		page, err := s.Page(name, body)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		for i, rec := range page.Records {
			check(name+" record "+string(rune('0'+i%10)), rec)
		}
	}
	ids := []string{"order-a", "order-b"}
	steps := s.PullSteps("2026-01-02", ids)
	text := strings.Join(steps, "\n")
	if len(steps) == 0 || !strings.Contains(text, "2026-01-02") {
		t.Errorf("plan steps don't name the window: %q", text)
	}
	for _, id := range ids {
		if !strings.Contains(text, id) {
			t.Errorf("plan steps omit open order %s", id)
		}
	}
	if s.FullPull() == "" || s.Lookup() == "" {
		t.Error("FullPull and Lookup must say how to pull")
	}
	if n == 0 {
		t.Fatal("conformance: no order records checked")
	}
	t.Logf("%s: %d order records conform", s.Name(), n)
}
