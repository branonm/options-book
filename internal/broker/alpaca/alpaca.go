// Package alpaca is the Alpaca adapter (PLAN.md item 6, step 4): it decodes
// Alpaca's own REST responses (trading API v2 orders, positions and account;
// the options contracts endpoint; market-data option snapshots, latest stock
// trades and bars) into internal/broker's canonical types. It is pure, like
// every decoder: bytes in, values out, no network, and it imports neither
// internal/rh nor internal/alpaca (the live paper client). A test fails the
// build if it ever does.
//
// Where Alpaca's data differs from the canonical shape, the adapter says so
// rather than guessing:
//   - An option's instrument id is its OCC symbol, as Alpaca writes it
//     (unpadded, e.g. AAA270617C00055000), in every record.
//   - Alpaca reports one aggregate fill per order or leg (filled_avg_price and
//     filled_qty), so each leg gets one Execution built from them. The order's
//     Premium is that price × quantity × the multiplier, which a sub-cent
//     average price makes a non-whole-cent total; the ledger refuses that as it
//     refuses any sub-cent amount.
//   - The order carries no contract size. Multiplier is "100", the standard
//     contract; Instruments reads the real size from the contracts endpoint.
//   - Canonical time in force is gfd/gtc, so Alpaca's "day" becomes "gfd".
//   - A position's pending buy quantity is not in Alpaca's positions
//     response; it reads empty ("not reported"), and the pending_buy_lag
//     check skips a position whose pending buys are not reported.
//   - Open interest is on the contract, not the snapshot, so an option quote
//     reads 0 and Instruments carries it in Instrument.OpenInterest;
//     snapshot.LoadOptionAt joins the two for a ticket.
//   - Alpaca does not publish minimum ticks or realized P&L: Ticks are empty
//     and RealizedPnL is an error.
package alpaca

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/shopspring/decimal"

	"github.com/branonm/options-book/internal/broker"
	"github.com/branonm/options-book/internal/occ"
)

// Name is the adapter's registry name, and the source an account names.
const Name = "alpaca"

// Decoder is the Alpaca decoder (broker.Decoder).
type Decoder struct{}

func init() { broker.Register(Decoder{}) }

func (Decoder) Name() string { return Name }

// num is a JSON string or bare number, kept as the text Alpaca sent.
type num struct{ text string }

func (n *num) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	switch {
	case s == "null":
		n.text = ""
	case strings.HasPrefix(s, `"`):
		return json.Unmarshal(b, &n.text)
	default:
		n.text = s
	}
	return nil
}

func (n num) N() broker.Num { return broker.N(n.text) }

// State maps Alpaca's order statuses onto canonical ones. A status the adapter
// doesn't know is StateUnknown, never a guess.
func State(s string) broker.OrderState {
	switch s {
	case "new", "partially_filled", "accepted", "pending_new", "accepted_for_bidding", "pending_cancel", "pending_replace", "held":
		return broker.StateOpen
	case "filled":
		return broker.StateFilled
	case "canceled", "expired", "replaced":
		return broker.StateCancelled
	case "rejected":
		return broker.StateRejected
	}
	return broker.StateUnknown
}

// Time in force, in the canonical words.
func tif(s string) string {
	if s == "day" {
		return "gfd"
	}
	return s
}

type rawLeg struct {
	Symbol         string `json:"symbol"`
	Side           string `json:"side"`
	PositionIntent string `json:"position_intent"`
	RatioQty       num    `json:"ratio_qty"`
	FilledQty      num    `json:"filled_qty"`
	FilledAvgPrice num    `json:"filled_avg_price"`
}

type rawOrder struct {
	ID             string   `json:"id"`
	CreatedAt      string   `json:"created_at"`
	UpdatedAt      string   `json:"updated_at"`
	FilledAt       string   `json:"filled_at"`
	Symbol         string   `json:"symbol"`
	AssetClass     string   `json:"asset_class"`
	OrderClass     string   `json:"order_class"`
	Qty            num      `json:"qty"`
	FilledQty      num      `json:"filled_qty"`
	FilledAvgPrice num      `json:"filled_avg_price"`
	LimitPrice     num      `json:"limit_price"`
	Side           string   `json:"side"`
	PositionIntent string   `json:"position_intent"`
	TimeInForce    string   `json:"time_in_force"`
	Status         string   `json:"status"`
	Legs           []rawLeg `json:"legs"`
}

// effect reads a position intent ("sell_to_open") into side and effect.
func intent(s string) (side, effect string, err error) {
	a, b, ok := strings.Cut(s, "_to_")
	if !ok || (a != broker.Buy && a != broker.Sell) || (b != broker.Open && b != broker.Close) {
		return "", "", fmt.Errorf("position_intent %q is not <buy|sell>_to_<open|close>", s)
	}
	return a, b, nil
}

func order(o rawOrder) (broker.Order, error) {
	if o.ID == "" || o.CreatedAt == "" || o.Status == "" {
		return broker.Order{}, fmt.Errorf("order record missing id, status or created_at")
	}
	if o.AssetClass != "" && o.AssetClass != "us_option" {
		return broker.Order{}, fmt.Errorf("order %s is a %s order, not an option order", o.ID, o.AssetClass)
	}
	legs := o.Legs
	if o.OrderClass != "mleg" {
		legs = []rawLeg{{Symbol: o.Symbol, Side: o.Side, PositionIntent: o.PositionIntent, RatioQty: num{"1"}, FilledQty: o.FilledQty, FilledAvgPrice: o.FilledAvgPrice}}
	}
	if len(legs) == 0 {
		return broker.Order{}, fmt.Errorf("order %s has no legs", o.ID)
	}
	out := broker.Order{
		ID: o.ID, State: State(o.Status), BrokerState: o.Status, Quantity: o.Qty.N(), FilledQuantity: o.FilledQty.N(),
		Price: o.LimitPrice.N(), Multiplier: broker.N("100"), TimeInForce: tif(o.TimeInForce), CreatedAt: o.CreatedAt,
	}
	mult := decimal.NewFromInt(100)
	premium := decimal.Zero
	for _, l := range legs {
		c, err := occ.Parse(l.Symbol)
		if err != nil {
			return broker.Order{}, fmt.Errorf("order %s: %w", o.ID, err)
		}
		if out.Symbol == "" {
			out.Symbol = c.Root
		}
		side, effect, err := intent(l.PositionIntent)
		if err != nil {
			return broker.Order{}, fmt.Errorf("order %s: %w", o.ID, err)
		}
		if l.Side != "" && l.Side != side {
			return broker.Order{}, fmt.Errorf("order %s: side %q disagrees with position_intent %q", o.ID, l.Side, l.PositionIntent)
		}
		ratio := 1
		if l.RatioQty.text != "" {
			if _, err := fmt.Sscan(l.RatioQty.text, &ratio); err != nil {
				return broker.Order{}, fmt.Errorf("order %s: ratio_qty %q", o.ID, l.RatioQty.text)
			}
		}
		leg := broker.Leg{InstrumentID: strings.ToUpper(strings.TrimSpace(l.Symbol)), Side: side, Effect: effect, Ratio: ratio,
			Expiry: c.Expiry, Strike: broker.N(c.Strike.String()), Right: c.Type}
		if fq, err := decimal.NewFromString(l.FilledQty.text); err == nil && fq.IsPositive() {
			price, err := decimal.NewFromString(l.FilledAvgPrice.text)
			if err != nil {
				return broker.Order{}, fmt.Errorf("order %s: filled_qty %s but filled_avg_price %q", o.ID, fq, l.FilledAvgPrice.text)
			}
			at := o.FilledAt
			if at == "" {
				at = o.UpdatedAt
			}
			leg.Executions = []broker.Execution{{ID: o.ID + "-fill", Price: l.FilledAvgPrice.N(), Quantity: l.FilledQty.N(), TradeDate: datePart(at), Time: at}}
			premium = premium.Add(price.Mul(fq).Mul(mult))
		}
		out.Legs = append(out.Legs, leg)
	}
	out.Premium = broker.N(premium.String())
	return out, nil
}

func datePart(ts string) string {
	if len(ts) >= 10 {
		return ts[:10]
	}
	return ts
}

func (Decoder) Orders(name string, body []byte) ([]broker.Order, error) {
	var raw []rawOrder
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	out := make([]broker.Order, 0, len(raw))
	for _, r := range raw {
		if r.AssetClass != "" && r.AssetClass != "us_option" {
			continue // an equity order is not in an option order log
		}
		o, err := order(r)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		out = append(out, o)
	}
	return out, nil
}

func (Decoder) OrderRecord(name string, record []byte) (broker.Order, error) {
	var r rawOrder
	if err := json.Unmarshal(record, &r); err != nil {
		return broker.Order{}, fmt.Errorf("%s: %w", name, err)
	}
	return order(r)
}

type rawPosition struct {
	Symbol        string `json:"symbol"`
	AssetClass    string `json:"asset_class"`
	Qty           num    `json:"qty"`
	Side          string `json:"side"`
	AvgEntryPrice num    `json:"avg_entry_price"`
}

func positions(name string, body []byte) ([]rawPosition, error) {
	var raw []rawPosition
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return raw, nil
}

func (Decoder) EquityPositions(name string, body []byte) ([]broker.EquityPosition, error) {
	raw, err := positions(name, body)
	if err != nil {
		return nil, err
	}
	var out []broker.EquityPosition
	for _, p := range raw {
		if p.AssetClass == "us_option" {
			continue
		}
		out = append(out, broker.EquityPosition{Symbol: p.Symbol, Quantity: p.Qty.N(), AverageCost: p.AvgEntryPrice.N(), Side: p.Side})
	}
	return out, nil
}

// OptionPositions: Alpaca's qty is negative for a short and avg_entry_price is
// per share; the canonical position has a positive count and a per-contract
// price, negative for a credit.
func (Decoder) OptionPositions(name string, body []byte) ([]broker.OptionPosition, error) {
	raw, err := positions(name, body)
	if err != nil {
		return nil, err
	}
	var out []broker.OptionPosition
	for _, p := range raw {
		if p.AssetClass != "us_option" {
			continue
		}
		c, err := occ.Parse(p.Symbol)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		q, err := decimal.NewFromString(p.Qty.text)
		if err != nil {
			return nil, fmt.Errorf("%s: %s quantity %q", name, p.Symbol, p.Qty.text)
		}
		price, err := decimal.NewFromString(p.AvgEntryPrice.text)
		if err != nil {
			return nil, fmt.Errorf("%s: %s avg_entry_price %q", name, p.Symbol, p.AvgEntryPrice.text)
		}
		per := price.Mul(decimal.NewFromInt(100))
		if p.Side == broker.Short {
			per = per.Neg()
		}
		out = append(out, broker.OptionPosition{InstrumentID: strings.ToUpper(strings.TrimSpace(p.Symbol)), Symbol: c.Root, Side: p.Side,
			Quantity: broker.N(q.Abs().String()), AveragePrice: broker.N(per.String()), Expiry: c.Expiry, PendingBuyQuantity: broker.Num{}})
	}
	return out, nil
}

// Balance reads GET /v2/account: portfolio_value, equity, cash and buying_power.
func (Decoder) Balance(name string, body []byte) (broker.Balance, error) {
	var a struct {
		PortfolioValue num `json:"portfolio_value"`
		Equity         num `json:"equity"`
		Cash           num `json:"cash"`
		BuyingPower    num `json:"buying_power"`
	}
	if err := json.Unmarshal(body, &a); err != nil {
		return broker.Balance{}, fmt.Errorf("%s: %w", name, err)
	}
	return broker.Balance{TotalValue: a.PortfolioValue.N(), EquityValue: a.Equity.N(), Cash: a.Cash.N(), BuyingPower: a.BuyingPower.N()}, nil
}

// EquityQuotes reads the data API's latest trades: {"trades":{"AAA":{"p":..,"t":..}}}.
func (Decoder) EquityQuotes(name string, body []byte) ([]broker.EquityQuote, error) {
	var r struct {
		Trades map[string]struct {
			P num    `json:"p"`
			T string `json:"t"`
		} `json:"trades"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	var out []broker.EquityQuote
	for _, sym := range sortedKeys(r.Trades) {
		t := r.Trades[sym]
		out = append(out, broker.EquityQuote{Symbol: sym, Last: t.P.N(), LastTradeAt: t.T})
	}
	return out, nil
}

// OptionQuotes reads the data API's option snapshots: {"snapshots":{OCC:{...}}}.
func (Decoder) OptionQuotes(name string, body []byte) ([]broker.OptionQuote, error) {
	var r struct {
		Snapshots map[string]struct {
			LatestQuote struct {
				BP num    `json:"bp"`
				AP num    `json:"ap"`
				T  string `json:"t"`
			} `json:"latestQuote"`
			Greeks struct {
				Delta num `json:"delta"`
			} `json:"greeks"`
			IV       num `json:"impliedVolatility"`
			DailyBar struct {
				V num `json:"v"`
			} `json:"dailyBar"`
		} `json:"snapshots"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	var out []broker.OptionQuote
	for _, sym := range sortedKeys(r.Snapshots) {
		s := r.Snapshots[sym]
		q := broker.OptionQuote{InstrumentID: strings.ToUpper(sym), Bid: s.LatestQuote.BP.N(), Ask: s.LatestQuote.AP.N(),
			Delta: s.Greeks.Delta.N(), ImpliedVolatility: s.IV.N(), UpdatedAt: s.LatestQuote.T}
		bid, e1 := decimal.NewFromString(s.LatestQuote.BP.text)
		ask, e2 := decimal.NewFromString(s.LatestQuote.AP.text)
		if e1 == nil && e2 == nil {
			q.Mark = broker.N(bid.Add(ask).Div(decimal.NewFromInt(2)).String()) // Alpaca sends no mark: the midpoint
		}
		if v, err := decimal.NewFromString(s.DailyBar.V.text); err == nil {
			q.Volume = v.IntPart()
		}
		out = append(out, q)
	}
	return out, nil
}

// Instruments reads the options contracts endpoint. A stored body with a
// next_page_token is an incomplete pull and is refused.
func (Decoder) Instruments(name string, body []byte) ([]broker.Instrument, error) {
	var r struct {
		Contracts []struct {
			Symbol     string `json:"symbol"`
			Status     string `json:"status"`
			Tradable   bool   `json:"tradable"`
			Expiration string `json:"expiration_date"`
			Root       string `json:"underlying_symbol"`
			Type       string `json:"type"`
			Strike     num    `json:"strike_price"`
			Size       num    `json:"size"`
			OI         num    `json:"open_interest"`
		} `json:"option_contracts"`
		Next *string `json:"next_page_token"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if r.Next != nil && *r.Next != "" {
		return nil, fmt.Errorf("%s: next_page_token is set, so this pull is incomplete", name)
	}
	var out []broker.Instrument
	for _, c := range r.Contracts {
		tr := "untradable"
		if c.Tradable {
			tr = "tradable"
		}
		in := broker.Instrument{ID: strings.ToUpper(c.Symbol), Symbol: c.Root, Expiry: c.Expiration, Strike: c.Strike.N(),
			Right: c.Type, State: c.Status, Tradability: tr, Multiplier: c.Size.N()}
		if oi, err := decimal.NewFromString(c.OI.text); err == nil {
			in.OpenInterest = oi.IntPart()
		}
		out = append(out, in)
	}
	return out, nil
}

func (Decoder) RealizedPnL(name string, body []byte) (broker.RealizedPnL, error) {
	return broker.RealizedPnL{}, fmt.Errorf("%s: Alpaca publishes no realized option P&L to reconcile against", name)
}

// Bars reads the data API's stock bars: {"bars":{"AAA":[{"t":..,"c":..}]}}.
func (Decoder) Bars(name string, body []byte) ([]broker.BarSeries, error) {
	var r struct {
		Bars map[string][]struct {
			T string `json:"t"`
			C num    `json:"c"`
		} `json:"bars"`
		Next *string `json:"next_page_token"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if r.Next != nil && *r.Next != "" {
		return nil, fmt.Errorf("%s: next_page_token is set, so this pull is incomplete", name)
	}
	var out []broker.BarSeries
	for _, sym := range sortedKeys(r.Bars) {
		bs := broker.BarSeries{Symbol: sym}
		for _, b := range r.Bars[sym] {
			bs.Bars = append(bs.Bars, broker.Bar{Start: b.T, Close: b.C.N()})
		}
		out = append(out, bs)
	}
	return out, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
