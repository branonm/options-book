// Package broker holds the canonical types every broker adapter decodes into
// (PLAN.md item 6, step 1): Order (multi-leg, with a canonical State), Leg,
// Execution, EquityPosition, OptionPosition, Balance, EquityQuote,
// OptionQuote, Instrument, RealizedPnL and Bars. Code outside an adapter
// works on these, never on a broker's own response shapes.
//
// Raw bytes stay the record; these are derived from them and never stored.
// So a number keeps the exact text the broker sent (Num): figures, findings
// and tickets print the same characters whichever adapter produced them,
// and each consumer parses and checks as it always has. Timestamps are the
// broker's RFC3339 strings. The broker's own name for an order's state is
// kept beside the canonical one, for display.
package broker

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/shopspring/decimal"
)

// Num is a number exactly as the broker sent it.
type Num struct{ Text string }

// N makes a Num from text.
func N(text string) Num { return Num{Text: text} }

// Dec parses the number (surrounding space ignored).
func (n Num) Dec() (decimal.Decimal, error) {
	d, err := decimal.NewFromString(strings.TrimSpace(n.Text))
	if err != nil {
		return decimal.Zero, fmt.Errorf("invalid decimal %q: %w", n.Text, err)
	}
	return d, nil
}

func (n Num) String() string { return n.Text }

// MarshalJSON writes the number as the string the broker sent, so a canonical
// value serializes like the response it came from.
func (n Num) MarshalJSON() ([]byte, error) { return json.Marshal(n.Text) }

// UnmarshalJSON reads a JSON string (or a bare number, keeping its text).
func (n *Num) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		return json.Unmarshal(b, &n.Text)
	}
	n.Text = string(b)
	return nil
}

// OrderState is an order's canonical state.
type OrderState string

const (
	StateOpen      OrderState = "open"      // working at the broker: may still fill
	StateFilled    OrderState = "filled"    // done, fully executed
	StateCancelled OrderState = "cancelled" // done, not (fully) executed
	StateRejected  OrderState = "rejected"
	StateFailed    OrderState = "failed"
	StateUnknown   OrderState = "unknown" // a state the adapter doesn't know; treated as neither open nor filled
)

// Canonical sides, effects, rights and position sides: the words the code compares against.
const (
	Buy   = "buy"
	Sell  = "sell"
	Open  = "open"
	Close = "close"
	Call  = "call"
	Put   = "put"
	Long  = "long"
	Short = "short"
)

// Execution is one fill of one leg.
type Execution struct {
	ID        string
	Price     Num
	Quantity  Num
	TradeDate string
	Time      string // RFC3339
}

// Leg is one contract of an order.
type Leg struct {
	InstrumentID string // the broker's option instrument id
	Side         string // Buy or Sell
	Effect       string // Open or Close
	Ratio        int
	Expiry       string // YYYY-MM-DD
	Strike       Num
	Right        string // Call or Put
	Executions   []Execution
}

// Order is an option order, single- or multi-leg.
type Order struct {
	ID              string
	Symbol          string // the underlying
	State           OrderState
	BrokerState     string // the broker's own word, e.g. Robinhood's "confirmed"
	Quantity        Num
	FilledQuantity  Num
	Price           Num // limit, per share
	Premium         Num // total cash processed so far
	Multiplier      Num
	TimeInForce     string // as the broker names it (gtc, gfd)
	OpeningStrategy string
	ClosingStrategy string
	CreatedAt       string // RFC3339
	Legs            []Leg
}

// IsOpen reports whether the order may still fill.
func (o Order) IsOpen() bool { return o.State == StateOpen }

// EquityPosition is a share holding.
type EquityPosition struct {
	Symbol      string
	Quantity    Num
	AverageCost Num // per share
	Side        string
}

// OptionPosition is an option holding.
type OptionPosition struct {
	InstrumentID       string `json:"instrument_id"`
	Symbol             string `json:"symbol"`
	Side               string `json:"side"` // Long or Short
	Quantity           Num    `json:"quantity"`
	AveragePrice       Num    `json:"average_price"` // per contract; negative for a credit
	Expiry             string `json:"expiry"`
	PendingBuyQuantity Num    `json:"pending_buy_quantity"`
}

// Balance is an account's portfolio values.
type Balance struct {
	TotalValue  Num
	EquityValue Num
	Cash        Num
	BuyingPower Num
}

// EquityQuote is one stock or fund quote.
type EquityQuote struct {
	Symbol      string
	Last        Num
	LastTradeAt string // RFC3339
	State       string
}

// OptionQuote is one contract's quote and Greeks.
type OptionQuote struct {
	InstrumentID      string
	Bid, Ask, Mark    Num
	Delta             Num
	ImpliedVolatility Num
	OpenInterest      int64
	Volume            int64
	UpdatedAt         string // RFC3339
}

// Ticks are a contract's price increments.
type Ticks struct {
	Above, Below, Cutoff Num
}

// Instrument is one option contract's definition.
type Instrument struct {
	ID          string
	Symbol      string
	Expiry      string
	Strike      Num
	Right       string
	State       string
	Tradability string
	Multiplier  Num
	Ticks       Ticks
	// OpenInterest is set by a source that keeps it on the contract rather than on
	// the quote (Alpaca). 0 means the instrument does not report it.
	OpenInterest int64
}

// RealizedPnL is the broker's own realized option P&L over a window.
type RealizedPnL struct {
	Window string // YYYY-MM-DD..YYYY-MM-DD
	Total  Num
}

// Bar is one period's bar.
type Bar struct {
	Start        string // RFC3339
	Close        Num
	Interpolated bool
	Session      string
}

// BarSeries is one symbol's bars.
type BarSeries struct {
	Symbol, Interval, Bounds string
	Bars                     []Bar
}

// Decoder turns one broker's saved responses into canonical types. It is
// pure: bytes in, values out, no network. name labels errors (a file path).
type Decoder interface {
	Name() string
	Orders(name string, body []byte) ([]Order, error)      // an order-list response
	OrderRecord(name string, record []byte) (Order, error) // one order record, as the order log holds it
	EquityPositions(name string, body []byte) ([]EquityPosition, error)
	OptionPositions(name string, body []byte) ([]OptionPosition, error)
	Balance(name string, body []byte) (Balance, error)
	EquityQuotes(name string, body []byte) ([]EquityQuote, error)
	OptionQuotes(name string, body []byte) ([]OptionQuote, error)
	Instruments(name string, body []byte) ([]Instrument, error)
	RealizedPnL(name string, body []byte) (RealizedPnL, error)
	Bars(name string, body []byte) ([]BarSeries, error)
}

var (
	mu       sync.Mutex
	decoders = map[string]Decoder{}
)

// Register makes an adapter's decoder available by name. Adapters call it
// from init; registering a name twice panics.
func Register(d Decoder) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := decoders[d.Name()]; dup {
		panic("broker: " + d.Name() + " registered twice")
	}
	decoders[d.Name()] = d
}

// ErrUnknownBroker is a name no adapter registered.
var ErrUnknownBroker = errors.New("no such broker adapter")

// Lookup returns a registered decoder.
func Lookup(name string) (Decoder, error) {
	mu.Lock()
	defer mu.Unlock()
	d, ok := decoders[name]
	if !ok {
		return nil, fmt.Errorf("%q: %w", name, ErrUnknownBroker)
	}
	return d, nil
}

// Names lists the registered adapters.
func Names() []string {
	mu.Lock()
	defer mu.Unlock()
	var out []string
	for n := range decoders {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Default names the adapter used wherever a broker isn't chosen yet. Every
// snapshot and order log in the repo is Robinhood's; config/accounts.json
// (item 6 step 3) will name the source per account, and this goes away.
const Default = "robinhood"

// OrderHead is the part of an order record an order log compares.
type OrderHead struct {
	ID, State, CreatedAt, UpdatedAt string
	// Version changes when the order meaningfully changes: its state, fills,
	// quantity, price or time in force. It deliberately excludes the broker's
	// housekeeping touches (Robinhood bumps updated_at on every resting GTC
	// order each morning), which must not add a log line.
	Version string
}

// OrderPage is one page of a saved order-list response.
type OrderPage struct {
	Records []json.RawMessage // each order record, exactly as the broker sent it
	Next    string            // the cursor for the next page; "" on the last page
}

// OrderSync is what an order log needs of a broker (PLAN.md item 6 step 2):
// how to read an order record's head, which states are final, how a list
// response pages, and what the next incremental pull must ask the broker for.
// It is pure, like Decoder: bytes in, values out.
type OrderSync interface {
	Name() string
	// Head reads a raw order record's head; a record without an id, state or
	// creation time is an error.
	Head(record []byte) (OrderHead, error)
	// Terminal reports whether an order in this state (the broker's own word)
	// can never change again.
	Terminal(state string) bool
	// Page reads one saved order-list response. It does not refuse a cursor:
	// the log checks that a pull's pages are complete.
	Page(name string, body []byte) (OrderPage, error)
	// FullPull says how to pull an account's entire history (the first pull).
	FullPull() string
	// PullSteps says, one line each, what the next incremental pull must
	// contain: a window of orders created since the given date, the open
	// orders, and a lookup for each log-open order in neither (openIDs).
	PullSteps(since string, openIDs []string) []string
	// Lookup says how to fetch one order by id.
	Lookup() string
}

var syncs = map[string]OrderSync{}

// RegisterSync makes an adapter's order sync available by name.
func RegisterSync(s OrderSync) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := syncs[s.Name()]; dup {
		panic("broker: order sync " + s.Name() + " registered twice")
	}
	syncs[s.Name()] = s
}

// LookupSync returns a registered order sync.
func LookupSync(name string) (OrderSync, error) {
	mu.Lock()
	defer mu.Unlock()
	s, ok := syncs[name]
	if !ok {
		return nil, fmt.Errorf("%q: %w", name, ErrUnknownBroker)
	}
	return s, nil
}

// EntryOrder is one order to be entered by hand or in a broker's web form.
type EntryOrder struct {
	Side   string // "sell" or "buy"
	Effect string // "open" or "close"
	Symbol string
	Right  string // "call" or "put"
	Strike string
	Expiry string
	Qty    int64
	Limit  string
	TIF    string // "gfd" or "gtc"
}

// EntryField is one field of a broker's order form, in the form's order.
type EntryField struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// EntryForm is the optional capability that knows a broker's web order form
// (PLAN.md addendum B): its fields, in its order. An adapter without one gets
// the generic order, and the ticket says so.
type EntryForm interface {
	Name() string
	EntryFields(o EntryOrder) []EntryField
}

var entryForms = map[string]EntryForm{}

// RegisterEntryForm makes an adapter's order form available by name.
func RegisterEntryForm(f EntryForm) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := entryForms[f.Name()]; dup {
		panic("broker: entry form " + f.Name() + " registered twice")
	}
	entryForms[f.Name()] = f
}

// LookupEntryForm returns a registered order form, or false when the adapter
// has none.
func LookupEntryForm(name string) (EntryForm, bool) {
	mu.Lock()
	defer mu.Unlock()
	f, ok := entryForms[name]
	return f, ok
}
