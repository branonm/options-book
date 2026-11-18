// Package diff reports what changed between two snapshots of the book:
// orders placed, filled and cancelled; legs opened and closed; realized
// premium; block premium and breakevens; holdings; and check-in findings.
package diff

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/shopspring/decimal"

	"github.com/branonm/options-book/internal/broker"
	"github.com/branonm/options-book/internal/format"
	"github.com/branonm/options-book/internal/ledger"
	"github.com/branonm/options-book/internal/rules"
	"github.com/branonm/options-book/internal/snapshot"
	"github.com/branonm/options-book/internal/strategy/wheel"
)

// Side is one end of a comparison.
type Side struct {
	Label string // e.g. "2026-11-04"
	Book  *wheel.Book
	Snap  *snapshot.Snapshot
}

// OrderChange is an order that is new, or whose state changed.
type OrderChange struct {
	Account, ID, Created, Leg, Action string
	Qty, Price, Premium               decimal.Decimal
	From, To, Tif                     string
}

// LegChange is a leg opened or closed between the snapshots.
type LegChange struct {
	Account, Leg, How string
	Qty               int64
	Net               decimal.Decimal
	Date              string
}

// BlockChange is a block whose shares, basis, premium or breakeven moved.
type BlockChange struct {
	Account, Symbol            string
	SharesFrom, SharesTo       int64
	BasisFrom, BasisTo         decimal.Decimal
	PremiumFrom, PremiumTo     decimal.Decimal
	BreakevenFrom, BreakevenTo decimal.Decimal
	Added, Removed             bool
}

// Report is the full comparison.
type Report struct {
	From, To     string
	Orders       []OrderChange
	Opened       []LegChange
	Closed       []LegChange
	Realized     map[string][2]decimal.Decimal // account → from, to
	Blocks       []BlockChange
	Resolved     []wheel.Finding
	New          []wheel.Finding
	accountOrder []string
}

// Compare builds the report. Both sides must use the same rules.
func Compare(from, to Side, r *rules.Rules) Report {
	rep := Report{From: from.Label, To: to.Label, Realized: map[string][2]decimal.Decimal{}}

	for _, ta := range to.Book.Accounts() {
		rep.accountOrder = append(rep.accountOrder, ta.Name)
		fa, _ := from.Book.Account(ta.Name)

		// Orders: by id.
		before := map[string]broker.Order{}
		if s := from.Snap.Accounts[ta.Name]; s != nil {
			for _, o := range s.Orders {
				before[o.ID] = o
			}
		}
		if s := to.Snap.Accounts[ta.Name]; s != nil {
			for _, o := range s.Orders {
				old, seen := before[o.ID]
				// The broker's own state word: confirmed -> partially_filled is a change even
				// though both are canonically open.
				if seen && old.BrokerState == o.BrokerState {
					continue
				}
				oc := orderChange(ta.Name, o)
				if seen {
					oc.From = old.BrokerState
				}
				rep.Orders = append(rep.Orders, oc)
			}
		}

		// Legs: an episode is identified by account, contract and open date.
		key := func(l ledger.Leg) string { return l.OptionID + "|" + l.OpenedAt }
		prev := map[string]ledger.Leg{}
		if fa != nil {
			for _, l := range fa.Ledger.Legs {
				prev[key(l)] = l
			}
		}
		for _, l := range ta.Ledger.Legs {
			if l.PreCampaign {
				continue
			}
			p, existed := prev[key(l)]
			switch {
			case l.End == ledger.EndOpen && !existed:
				rep.Opened = append(rep.Opened, LegChange{ta.Name, l.Key.String(), "sold to open", l.OpenQty, l.Credit, l.OpenedAt})
			case l.End != ledger.EndOpen && (!existed || p.End == ledger.EndOpen):
				how := map[string]string{ledger.EndBought: "bought back", ledger.EndExpired: "expired (outcome not recorded)", ledger.EndAssigned: "assigned", ledger.EndWorthless: "expired worthless"}[l.End]
				if l.Kind == ledger.KindAcquisitionPut {
					how = "assigned (acquisition put; credit to basis)"
				}
				rep.Closed = append(rep.Closed, LegChange{ta.Name, l.Key.String(), how, l.Qty, l.Net, l.ClosedAt})
			}
		}

		var fr decimal.Decimal
		if fa != nil {
			fr = fa.Ledger.Realized
		}
		rep.Realized[ta.Name] = [2]decimal.Decimal{fr, ta.Ledger.Realized}

		// Blocks.
		fb := map[string]*wheel.Block{}
		if fa != nil {
			for _, b := range fa.Blocks {
				fb[b.Symbol] = b
			}
		}
		seen := map[string]bool{}
		for _, b := range ta.Blocks {
			seen[b.Symbol] = true
			o := fb[b.Symbol]
			if o == nil {
				rep.Blocks = append(rep.Blocks, BlockChange{Account: ta.Name, Symbol: b.Symbol, Added: true,
					SharesTo: b.Shares, BasisTo: b.Basis, PremiumTo: b.Premium, BreakevenTo: b.Breakeven})
				continue
			}
			if o.Shares != b.Shares || !o.Basis.Equal(b.Basis) || !o.Premium.Equal(b.Premium) {
				rep.Blocks = append(rep.Blocks, BlockChange{Account: ta.Name, Symbol: b.Symbol,
					SharesFrom: o.Shares, SharesTo: b.Shares, BasisFrom: o.Basis, BasisTo: b.Basis,
					PremiumFrom: o.Premium, PremiumTo: b.Premium, BreakevenFrom: o.Breakeven, BreakevenTo: b.Breakeven})
			}
		}
		for sym, o := range fb {
			if !seen[sym] {
				rep.Blocks = append(rep.Blocks, BlockChange{Account: ta.Name, Symbol: sym, Removed: true,
					SharesFrom: o.Shares, BasisFrom: o.Basis, PremiumFrom: o.Premium, BreakevenFrom: o.Breakeven})
			}
		}
	}

	// Check-in findings. The key includes the expected value but not the live
	// one: a closer repriced but still wrong is the same finding (its expected
	// price comes from the fill), while a new leg on the same contract is not.
	fk := func(f wheel.Finding) string { return f.Account + "|" + f.Check + "|" + f.Subject + "|" + f.Expected }
	a, b := from.Book.Check(r), to.Book.Check(r)
	inA, inB := map[string]bool{}, map[string]bool{}
	for _, f := range append(a.Defects, a.Alerts...) {
		inA[fk(f)] = true
	}
	for _, f := range append(b.Defects, b.Alerts...) {
		inB[fk(f)] = true
		if !inA[fk(f)] {
			rep.New = append(rep.New, f)
		}
	}
	for _, f := range append(a.Defects, a.Alerts...) {
		if !inB[fk(f)] {
			rep.Resolved = append(rep.Resolved, f)
		}
	}

	sort.SliceStable(rep.Orders, func(i, j int) bool {
		if rep.Orders[i].Account != rep.Orders[j].Account {
			return rep.Orders[i].Account < rep.Orders[j].Account
		}
		return rep.Orders[i].Created < rep.Orders[j].Created
	})
	byDate := func(ls []LegChange) {
		sort.SliceStable(ls, func(i, j int) bool { return ls[i].Account+ls[i].Date < ls[j].Account+ls[j].Date })
	}
	byDate(rep.Opened)
	byDate(rep.Closed)
	sort.SliceStable(rep.Blocks, func(i, j int) bool {
		return rep.Blocks[i].Account+rep.Blocks[i].Symbol < rep.Blocks[j].Account+rep.Blocks[j].Symbol
	})
	return rep
}

func orderChange(acct string, o broker.Order) OrderChange {
	oc := OrderChange{Account: acct, ID: o.ID, Created: o.CreatedAt, To: o.BrokerState, Tif: o.TimeInForce}
	oc.Qty, _ = decimal.NewFromString(o.Quantity.Text)
	oc.Price, _ = decimal.NewFromString(o.Price.Text)
	oc.Premium, _ = decimal.NewFromString(o.Premium.Text)
	if len(o.Legs) == 1 {
		l := o.Legs[0]
		strike, _ := decimal.NewFromString(l.Strike.Text)
		oc.Leg = ledger.ContractKey{Symbol: o.Symbol, Type: l.Right, Strike: strike.String(), Expiry: l.Expiry}.String()
		verb := map[string]string{"sell/open": "sell to open", "buy/close": "buy to close", "buy/open": "buy to open", "sell/close": "sell to close"}
		oc.Action = verb[l.Side+"/"+l.Effect]
	} else {
		oc.Leg, oc.Action = o.Symbol, fmt.Sprintf("%d-leg order", len(o.Legs))
	}
	return oc
}

// Markdown writes the report.
func (rep Report) Markdown(w io.Writer) {
	p := func(f string, a ...any) { fmt.Fprintf(w, f, a...) }
	table := func(head []string, rows [][]string) {
		p("| %s |\n|%s\n", strings.Join(head, " | "), strings.Repeat("---|", len(head)))
		for _, r := range rows {
			p("| %s |\n", strings.Join(r, " | "))
		}
		p("\n")
	}
	p("## What changed: %s → %s\n\n", rep.From, rep.To)

	var real []string
	total := [2]decimal.Decimal{}
	for _, a := range rep.accountOrder {
		v := rep.Realized[a]
		total[0], total[1] = total[0].Add(v[0]), total[1].Add(v[1])
		real = append(real, fmt.Sprintf("%s %s → %s (%s)", a, format.USD0(v[0]), format.USD0(v[1]), format.Signed(v[1].Sub(v[0]))))
	}
	p("**Realized premium:** %s · **combined %s → %s (%s)**\n\n", strings.Join(real, " · "), format.USD0(total[0]), format.USD0(total[1]), format.Signed(total[1].Sub(total[0])))

	if len(rep.Closed) > 0 {
		var rows [][]string
		for _, l := range rep.Closed {
			rows = append(rows, []string{l.Account, l.Leg, fmt.Sprintf("×%d", l.Qty), l.How, l.Date, format.Signed(l.Net)})
		}
		p("### Legs closed\n\n")
		table([]string{"Acct", "Leg", "Qty", "How", "Date", "Net"}, rows)
	}
	if len(rep.Opened) > 0 {
		var rows [][]string
		for _, l := range rep.Opened {
			rows = append(rows, []string{l.Account, l.Leg, fmt.Sprintf("×%d", l.Qty), l.Date, format.USD0(l.Net)})
		}
		p("### Legs opened\n\n")
		table([]string{"Acct", "Leg", "Qty", "Date", "Credit"}, rows)
	}
	if len(rep.Orders) > 0 {
		var rows [][]string
		for _, o := range rep.Orders {
			state := o.To
			if o.From != "" {
				state = o.From + " → " + o.To
			} else {
				state = "new, " + o.To
			}
			prem := "—"
			if o.Premium.IsPositive() {
				prem = format.USD0(o.Premium)
			}
			rows = append(rows, []string{o.Account, o.Created[:16], o.Action, o.Leg, o.Qty.String(), format.Price(o.Price), "`" + o.Tif + "`", state, prem, o.ID[:8]})
		}
		p("### Orders\n\n")
		table([]string{"Acct", "Created (UTC)", "Action", "Contract", "Qty", "Limit", "tif", "State", "Filled", "Id"}, rows)
	}
	if len(rep.Blocks) > 0 {
		var rows [][]string
		for _, b := range rep.Blocks {
			switch {
			case b.Added:
				rows = append(rows, []string{b.Account, b.Symbol, fmt.Sprintf("new: %d @ %s", b.SharesTo, format.Price(b.BasisTo)), format.USD0(b.PremiumTo), format.Price(b.BreakevenTo)})
			case b.Removed:
				rows = append(rows, []string{b.Account, b.Symbol, fmt.Sprintf("gone (was %d @ %s)", b.SharesFrom, format.Price(b.BasisFrom)), "—", "—"})
			default:
				sh := fmt.Sprint(b.SharesTo)
				if b.SharesFrom != b.SharesTo || !b.BasisFrom.Equal(b.BasisTo) {
					sh = fmt.Sprintf("%d @ %s → %d @ %s", b.SharesFrom, format.Price(b.BasisFrom), b.SharesTo, format.Price(b.BasisTo))
				}
				rows = append(rows, []string{b.Account, b.Symbol, sh,
					fmt.Sprintf("%s → %s", format.USD0(b.PremiumFrom), format.USD0(b.PremiumTo)),
					fmt.Sprintf("%s → %s", format.Price(b.BreakevenFrom), format.Price(b.BreakevenTo))})
			}
		}
		p("### Blocks\n\n")
		table([]string{"Acct", "Symbol", "Shares", "Premium", "Breakeven"}, rows)
	}
	finding := func(title string, fs []wheel.Finding) {
		if len(fs) == 0 {
			return
		}
		var rows [][]string
		for _, f := range fs {
			rows = append(rows, []string{f.Account, f.Severity, f.Check, f.Subject, f.Expected, f.Actual})
		}
		p("### %s\n\n", title)
		table([]string{"Acct", "Kind", "Check", "Subject", "Expected", "Actual"}, rows)
	}
	finding("Check-in findings resolved", rep.Resolved)
	finding("New check-in findings", rep.New)
	if len(rep.Orders)+len(rep.Opened)+len(rep.Closed)+len(rep.Blocks)+len(rep.Resolved)+len(rep.New) == 0 {
		p("Nothing changed.\n")
	}
}
