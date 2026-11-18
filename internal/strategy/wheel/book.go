// Package wheel is the domain model of the wheel book:
//
//	Book → Pool → Account → Block → ShortCall → Closer
//	                      → ShortPut → Closer
//
// It is assembled from ledger output (legs, premium, breakevens) plus the
// rest of a snapshot (open orders, option positions, quotes). Each type knows
// its own rules: ExpectedCloser says what the book should hold, Check compares
// that with what it does hold. The ticket builder will reuse these types for
// proposed trades.
package wheel

import (
	"fmt"
	"sort"
	"time"

	"github.com/shopspring/decimal"

	"github.com/branonm/options-book/internal/broker"
	"github.com/branonm/options-book/internal/config"
	"github.com/branonm/options-book/internal/ledger"
	"github.com/branonm/options-book/internal/rules"
	"github.com/branonm/options-book/internal/snapshot"
)

// Closer is a resting buy-to-close order against a short option.
type Closer struct {
	OrderID string          `json:"order_id"`
	Qty     int64           `json:"qty"`
	Price   decimal.Decimal `json:"price"`
	Tif     string          `json:"time_in_force"`
	State   string          `json:"state"`
}

// ShortCall is an open covered-call leg.
type ShortCall struct {
	Leg     ledger.Leg      `json:"leg"`
	Strike  decimal.Decimal `json:"strike"`
	Expiry  time.Time       `json:"expiry"`
	Closers []Closer        `json:"closers"`
	// Position is Robinhood's view of the same contract, when saved.
	Position *broker.OptionPosition `json:"-"`
	// Quote is the contract quote from option_quotes.json, when saved.
	Quote *broker.OptionQuote `json:"-"`
	block *Block
}

// ShortPut is an open short put. Rule 4a: acquisition is the default.
type ShortPut struct {
	Leg      ledger.Leg             `json:"leg"`
	Closers  []Closer               `json:"closers"`
	Position *broker.OptionPosition `json:"-"`
}

// Block is a share holding with the calls written against it.
type Block struct {
	ledger.Block
	Price    decimal.Decimal `json:"price"` // last trade, from the snapshot's quotes
	HasPrice bool            `json:"has_price"`
	Calls    []*ShortCall    `json:"calls"`
}

// Value is the block's market value at the snapshot quote.
func (b *Block) Value() decimal.Decimal { return b.Price.Mul(decimal.NewFromInt(b.Shares)) }

// Account is one brokerage account.
type Account struct {
	Name     string                `json:"name"`
	Label    string                `json:"label"`
	Pool     string                `json:"pool"`
	Expected []string              `json:"expected_symbols"`
	Ledger   *ledger.AccountLedger `json:"ledger"`
	Blocks   []*Block              `json:"blocks"`
	Puts     []*ShortPut           `json:"puts"`
	// Calls written on symbols with no shares (should never happen in an IRA).
	Uncovered []*ShortCall `json:"uncovered_calls,omitempty"`
	// Option positions Robinhood reports that no open leg explains.
	Orphans []broker.OptionPosition `json:"orphan_positions,omitempty"`
	// Inputs that were missing from the snapshot, so some checks could not run.
	openOrdersKnown bool
	positionsKnown  bool
	portfolio       *broker.Balance
}

// Pool is the unit risk percentages are computed over (the rulebook Part A).
type Pool struct {
	Name     string     `json:"name"`
	Accounts []*Account `json:"accounts"`
}

// Book is the whole wheel book at one moment.
type Book struct {
	AsOf      time.Time `json:"as_of"`
	QuoteTime string    `json:"quote_time"` // latest equity quote in the snapshot
	Pools     []*Pool   `json:"pools"`
	quotes    bool
}

// Assemble builds the book from a snapshot. It runs the ledger for each account.
func Assemble(cfg *config.Book, snap *snapshot.Snapshot, asOf time.Time) (*Book, error) {
	bk := &Book{AsOf: asOf, QuoteTime: snap.QuoteTime, quotes: snap.HasQuote}
	pools := map[string]*Pool{}
	for _, ac := range cfg.Accounts {
		raw, ok := snap.Accounts[ac.Name]
		if !ok {
			continue
		}
		l, err := ledger.Build(ledger.Input{
			Account:       ac.Name,
			AsOf:          asOf.Format("2006-01-02"),
			CampaignStart: cfg.CampaignStart,
			BlockStarts:   ac.BlockStarts,
			Orders:        raw.Orders,
			Equity:        raw.Equity,
			Expirations:   snap.Expirations,
		})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", ac.Name, err)
		}
		a, err := assembleAccount(ac, l, raw, snap)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", ac.Name, err)
		}
		p := pools[a.Pool]
		if p == nil {
			p = &Pool{Name: a.Pool}
			pools[a.Pool] = p
			bk.Pools = append(bk.Pools, p)
		}
		p.Accounts = append(p.Accounts, a)
	}
	return bk, nil
}

func assembleAccount(ac config.Account, l *ledger.AccountLedger, raw *snapshot.Account, snap *snapshot.Snapshot) (*Account, error) {
	pool := ac.Pool
	if pool == "" {
		pool = ac.Name
	}
	a := &Account{Name: ac.Name, Label: ac.Label, Pool: pool, Expected: ac.ExpectedSymbols, Ledger: l,
		openOrdersKnown: raw.OrdersAllStates, positionsKnown: raw.HasPositions, portfolio: raw.Portfolio}

	closers := map[string][]Closer{} // option_id
	for _, o := range raw.Orders {
		if !o.IsOpen() || len(o.Legs) != 1 {
			continue
		}
		lg := o.Legs[0]
		if lg.Side != broker.Buy || lg.Effect != broker.Close {
			continue
		}
		q, err := decimal.NewFromString(o.Quantity.Text)
		if err != nil || !q.IsInteger() {
			return nil, fmt.Errorf("open order %s: bad quantity %q", o.ID, o.Quantity.Text)
		}
		pq, _ := decimal.NewFromString(o.FilledQuantity.Text)
		p, err := decimal.NewFromString(o.Price.Text)
		if err != nil {
			return nil, fmt.Errorf("open order %s: bad price %q", o.ID, o.Price.Text)
		}
		closers[lg.InstrumentID] = append(closers[lg.InstrumentID], Closer{
			OrderID: o.ID, Qty: q.Sub(pq).IntPart(), Price: p, Tif: o.TimeInForce, State: o.BrokerState})
	}

	positions := map[string]*broker.OptionPosition{}
	for i := range raw.OptionPositions {
		positions[raw.OptionPositions[i].InstrumentID] = &raw.OptionPositions[i]
	}

	blocks := map[string]*Block{}
	for _, lb := range l.Blocks {
		b := &Block{Block: lb}
		b.Price, b.HasPrice = snap.Quotes[lb.Symbol]
		blocks[lb.Symbol] = b
		a.Blocks = append(a.Blocks, b)
	}

	used := map[string]bool{}
	for _, lg := range l.Legs {
		if lg.End != ledger.EndOpen || lg.PreCampaign {
			continue
		}
		used[lg.OptionID] = true
		pos := positions[lg.OptionID]
		switch lg.Key.Type {
		case "call":
			strike, err := decimal.NewFromString(lg.Key.Strike)
			if err != nil {
				return nil, err
			}
			exp, err := time.Parse("2006-01-02", lg.Key.Expiry)
			if err != nil {
				return nil, err
			}
			c := &ShortCall{Leg: lg, Strike: strike, Expiry: exp, Closers: closers[lg.OptionID], Position: pos}
			if q, ok := snap.OptionQuotes[lg.OptionID]; ok {
				c.Quote = &q
			}
			if b := blocks[lg.Key.Symbol]; b != nil {
				c.block = b
				b.Calls = append(b.Calls, c)
			} else {
				a.Uncovered = append(a.Uncovered, c)
			}
		case "put":
			a.Puts = append(a.Puts, &ShortPut{Leg: lg, Closers: closers[lg.OptionID], Position: pos})
		}
	}
	for id, p := range positions {
		if !used[id] {
			a.Orphans = append(a.Orphans, *p)
		}
	}
	sort.Slice(a.Orphans, func(i, j int) bool { return a.Orphans[i].InstrumentID < a.Orphans[j].InstrumentID })
	return a, nil
}

// Value is the account's share value at snapshot quotes.
func (a *Account) Value() decimal.Decimal {
	v := decimal.Zero
	for _, b := range a.Blocks {
		v = v.Add(b.Value())
	}
	return v
}

// CapValue is the value of blocks whose symbol is in the cap's group.
func (a *Account) CapValue(c rules.ConcentrationCap) decimal.Decimal {
	v := decimal.Zero
	for _, b := range a.Blocks {
		if c.Has(b.Symbol) {
			v = v.Add(b.Value())
		}
	}
	return v
}

// CapShare is the cap group's value and the pool's total share value.
func (p *Pool) CapShare(c rules.ConcentrationCap) (group, total decimal.Decimal) {
	for _, a := range p.Accounts {
		group = group.Add(a.CapValue(c))
		total = total.Add(a.Value())
	}
	return group, total
}

// Accounts lists every account in the book, in config order.
func (bk *Book) Accounts() []*Account {
	var out []*Account
	for _, p := range bk.Pools {
		out = append(out, p.Accounts...)
	}
	return out
}

// OpenPremium is what the open short calls are worth to the ledger: gross
// credit still open, and what remains after buying every call back at its
// live closers and at the Rule 10 closers.
type OpenPremium struct {
	Gross         decimal.Decimal `json:"gross"`
	NetAtLive     decimal.Decimal `json:"net_at_live_closers"`
	NetAtRule10   decimal.Decimal `json:"net_at_rule10_closers"`
	MissingLive   int             `json:"calls_without_live_closer"`
	RealizedSoFar decimal.Decimal `json:"realized"`
}

func (bk *Book) OpenPremium(r *rules.Rules) OpenPremium {
	var op OpenPremium
	mult := decimal.NewFromInt(r.LotSize.SharesPerContract)
	for _, a := range bk.Accounts() {
		op.RealizedSoFar = op.RealizedSoFar.Add(a.Ledger.Realized)
		for _, b := range a.Blocks {
			for _, c := range b.Calls {
				op.Gross = op.Gross.Add(c.Leg.Net)
				want := c.ExpectedCloser(r)
				op.NetAtRule10 = op.NetAtRule10.Add(c.Leg.Net.Sub(want.Price.Mul(decimal.NewFromInt(want.Qty)).Mul(mult)))
				if len(c.Closers) == 0 {
					op.MissingLive++
					continue
				}
				live := c.Leg.Net
				for _, cl := range c.Closers {
					live = live.Sub(cl.Price.Mul(decimal.NewFromInt(cl.Qty)).Mul(mult))
				}
				op.NetAtLive = op.NetAtLive.Add(live)
			}
		}
	}
	return op
}

// Ladder counts open short-call contracts per expiration across the book.
func (bk *Book) Ladder() []Rung {
	m := map[string]int64{}
	for _, a := range bk.Accounts() {
		for _, b := range a.Blocks {
			for _, c := range b.Calls {
				m[c.Leg.Key.Expiry] += c.Leg.OpenQty
			}
		}
	}
	var out []Rung
	for e, n := range m {
		out = append(out, Rung{Expiry: e, Contracts: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Expiry < out[j].Expiry })
	return out
}

type Rung struct {
	Expiry    string `json:"expiry"`
	Contracts int64  `json:"contracts"`
}

// callAway is what assignment at the strike pays on `called` shares of a
// block, as separate components. premium is the block's cumulative net
// premium including the leg in question; it is attributed pro rata.
func callAway(strike, basis, premium decimal.Decimal, shares, called int64, days int) *CallAway {
	if called <= 0 || shares <= 0 || days <= 0 {
		return nil
	}
	c := decimal.NewFromInt(called)
	ca := &CallAway{SharesCalled: called, Days: days,
		ShareGain: strike.Sub(basis).Mul(c),
		Premium:   premium.Mul(c).DivRound(decimal.NewFromInt(shares), 2),
		Capital:   basis.Mul(c)}
	ca.Total = ca.ShareGain.Add(ca.Premium)
	ca.Annualized = ca.Total.Div(ca.Capital).Mul(decimal.NewFromInt(365)).DivRound(decimal.NewFromInt(int64(days)), ledger.DivPlaces)
	return ca
}

// CallAway is what this live call pays if assigned at expiration.
func (c *ShortCall) CallAway(r *rules.Rules, bk *Book) *CallAway {
	if c.block == nil {
		return nil
	}
	return callAway(c.Strike, c.block.Basis, c.block.Premium, c.block.Shares, c.Leg.OpenQty*r.LotSize.SharesPerContract, c.DTE(bk))
}

// Portfolio is the account's saved get_portfolio response, or nil.
func (a *Account) Portfolio() *broker.Balance { return a.portfolio }
