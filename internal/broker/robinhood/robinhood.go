// Package robinhood is the Robinhood adapter (PLAN.md item 6): it decodes
// saved Robinhood MCP responses into internal/broker's canonical types. It is
// the only package that imports internal/rh, Robinhood's own response shapes.
//
// Decoding changes no value: every number keeps Robinhood's text, and the
// response checks are rh's (a paginated response with its next cursor set is
// refused). The wheel ledger still checks a filled order's executions
// against its processed totals.
package robinhood

import (
	"encoding/json"
	"fmt"

	"github.com/branonm/options-book/internal/broker"
	"github.com/branonm/options-book/internal/rh"
)

// Name is the adapter's registry name.
const Name = "robinhood"

// Decoder is the Robinhood decoder.
type Decoder struct{}

func init() { broker.Register(Decoder{}) }

func (Decoder) Name() string { return Name }

// State maps Robinhood's order states onto canonical ones.
func State(s string) broker.OrderState {
	switch s {
	case "queued", "unconfirmed", "confirmed", "partially_filled", "pending_cancelled":
		return broker.StateOpen
	case "filled":
		return broker.StateFilled
	case "cancelled", "voided":
		return broker.StateCancelled
	case "rejected":
		return broker.StateRejected
	case "failed":
		return broker.StateFailed
	}
	return broker.StateUnknown
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// Order converts one Robinhood order record.
func Order(o rh.OptionOrder) broker.Order {
	out := broker.Order{
		ID: o.ID, Symbol: o.ChainSymbol, State: State(o.State), BrokerState: o.State,
		Quantity: broker.N(o.Quantity), FilledQuantity: broker.N(o.ProcessedQuantity),
		Price: broker.N(o.Price), Premium: broker.N(o.ProcessedPremium), Multiplier: broker.N(o.TradeValueMultiplier),
		TimeInForce: o.TimeInForce, OpeningStrategy: str(o.OpeningStrategy), ClosingStrategy: str(o.ClosingStrategy),
		CreatedAt: o.CreatedAt,
	}
	for _, l := range o.Legs {
		leg := broker.Leg{
			InstrumentID: l.OptionID, Side: l.Side, Effect: l.PositionEffect, Ratio: l.RatioQuantity,
			Expiry: l.ExpirationDate, Strike: broker.N(l.StrikePrice), Right: l.OptionType,
		}
		for _, e := range l.Executions {
			leg.Executions = append(leg.Executions, broker.Execution{
				ID: e.ID, Price: broker.N(e.Price), Quantity: broker.N(e.Quantity), TradeDate: e.TradeDate, Time: e.Timestamp,
			})
		}
		out.Legs = append(out.Legs, leg)
	}
	return out
}

// Orders converts Robinhood order records.
func Orders(os []rh.OptionOrder) []broker.Order {
	out := make([]broker.Order, 0, len(os))
	for _, o := range os {
		out = append(out, Order(o))
	}
	return out
}

func (Decoder) Orders(name string, body []byte) ([]broker.Order, error) {
	var r rh.OptionOrdersResponse
	if err := rh.DecodeJSON(name, body, &r); err != nil {
		return nil, err
	}
	return Orders(r.Data.Orders), nil
}

func (Decoder) OrderRecord(name string, record []byte) (broker.Order, error) {
	var o rh.OptionOrder
	if err := json.Unmarshal(record, &o); err != nil {
		return broker.Order{}, err
	}
	return Order(o), nil
}

func (Decoder) EquityPositions(name string, body []byte) ([]broker.EquityPosition, error) {
	var r rh.EquityPositionsResponse
	if err := rh.DecodeJSON(name, body, &r); err != nil {
		return nil, err
	}
	return Equity(r.Data.Positions), nil
}

func (Decoder) OptionPositions(name string, body []byte) ([]broker.OptionPosition, error) {
	var r rh.OptionPositionsResponse
	if err := rh.DecodeJSON(name, body, &r); err != nil {
		return nil, err
	}
	out := make([]broker.OptionPosition, 0, len(r.Data.Positions))
	for _, p := range r.Data.Positions {
		out = append(out, broker.OptionPosition{
			InstrumentID: p.OptionID, Symbol: p.ChainSymbol, Side: p.Type, Quantity: broker.N(p.Quantity),
			AveragePrice: broker.N(p.AveragePrice), Expiry: p.ExpirationDate, PendingBuyQuantity: broker.N(p.PendingBuyQuantity),
		})
	}
	return out, nil
}

func (Decoder) Balance(name string, body []byte) (broker.Balance, error) {
	var r rh.PortfolioResponse
	if err := rh.DecodeJSON(name, body, &r); err != nil {
		return broker.Balance{}, err
	}
	p := r.Data
	return broker.Balance{TotalValue: broker.N(p.TotalValue), EquityValue: broker.N(p.EquityValue), Cash: broker.N(p.Cash), BuyingPower: broker.N(p.BuyingPower.BuyingPower)}, nil
}

func (Decoder) EquityQuotes(name string, body []byte) ([]broker.EquityQuote, error) {
	var r rh.EquityQuotesResponse
	if err := rh.DecodeJSON(name, body, &r); err != nil {
		return nil, err
	}
	out := make([]broker.EquityQuote, 0, len(r.Data.Results))
	for _, x := range r.Data.Results {
		q := x.Quote
		out = append(out, broker.EquityQuote{Symbol: q.Symbol, Last: broker.N(q.LastTradePrice), LastTradeAt: q.VenueLastTradeTime, State: q.State})
	}
	return out, nil
}

// OptionQuote converts one Robinhood option quote.
func OptionQuote(q rh.OptionQuote) broker.OptionQuote {
	return broker.OptionQuote{
		InstrumentID: q.InstrumentID, Bid: broker.N(q.BidPrice), Ask: broker.N(q.AskPrice), Mark: broker.N(q.MarkPrice),
		Delta: broker.N(q.Delta), ImpliedVolatility: broker.N(q.ImpliedVolatility),
		OpenInterest: q.OpenInterest, Volume: q.Volume, UpdatedAt: q.UpdatedAt,
	}
}

func (Decoder) OptionQuotes(name string, body []byte) ([]broker.OptionQuote, error) {
	var r rh.OptionQuotesResponse
	if err := rh.DecodeJSON(name, body, &r); err != nil {
		return nil, err
	}
	out := make([]broker.OptionQuote, 0, len(r.Data.Results))
	for _, x := range r.Data.Results {
		out = append(out, OptionQuote(x.Quote))
	}
	return out, nil
}

// Instrument converts one Robinhood option instrument.
func Instrument(in rh.OptionInstrument) broker.Instrument {
	return broker.Instrument{
		ID: in.ID, Symbol: in.ChainSymbol, Expiry: in.ExpirationDate, Strike: broker.N(in.StrikePrice), Right: in.Type,
		State: in.State, Tradability: in.Tradability, Multiplier: broker.N(in.TradeValueMultiplier),
		Ticks: broker.Ticks{Above: broker.N(in.MinTicks.AboveTick), Below: broker.N(in.MinTicks.BelowTick), Cutoff: broker.N(in.MinTicks.CutoffPrice)},
	}
}

func (Decoder) Instruments(name string, body []byte) ([]broker.Instrument, error) {
	var r rh.OptionInstrumentsResponse
	if err := rh.DecodeJSON(name, body, &r); err != nil {
		return nil, err
	}
	out := make([]broker.Instrument, 0, len(r.Data.Instruments))
	for _, in := range r.Data.Instruments {
		out = append(out, Instrument(in))
	}
	return out, nil
}

func (Decoder) RealizedPnL(name string, body []byte) (broker.RealizedPnL, error) {
	var r rh.RealizedPnLResponse
	if err := rh.DecodeJSON(name, body, &r); err != nil {
		return broker.RealizedPnL{}, err
	}
	return broker.RealizedPnL{Window: r.Data.Window, Total: broker.N(r.Data.TotalReturns)}, nil
}

func (Decoder) Bars(name string, body []byte) ([]broker.BarSeries, error) {
	var r rh.EquityHistoricalsResponse
	if err := rh.DecodeJSON(name, body, &r); err != nil {
		return nil, err
	}
	var out []broker.BarSeries
	for _, s := range r.Data.Results {
		bs := broker.BarSeries{Symbol: s.Symbol, Interval: s.Interval, Bounds: s.Bounds}
		for _, b := range s.Bars {
			bs.Bars = append(bs.Bars, broker.Bar{Start: b.BeginsAt, Close: broker.N(b.ClosePrice), Interpolated: b.Interpolated, Session: b.Session})
		}
		out = append(out, bs)
	}
	return out, nil
}

// Equity converts Robinhood equity positions.
func Equity(ps []rh.EquityPosition) []broker.EquityPosition {
	out := make([]broker.EquityPosition, 0, len(ps))
	for _, p := range ps {
		out = append(out, broker.EquityPosition{Symbol: p.Symbol, Quantity: broker.N(p.Quantity), AverageCost: broker.N(p.AverageBuyPrice), Side: p.Type})
	}
	return out
}

// InstrumentsPage decodes one page of a paged get_option_instruments pull. A
// page legitimately carries a next cursor (rhmcp.Client.Pages follows it), so
// unlike Decoder.Instruments, which refuses a stored body with a cursor set,
// this doesn't. The server's pull paths use it; a stored body never does.
func InstrumentsPage(name string, body []byte) ([]broker.Instrument, error) {
	var r rh.OptionInstrumentsResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	out := make([]broker.Instrument, 0, len(r.Data.Instruments))
	for _, in := range r.Data.Instruments {
		out = append(out, Instrument(in))
	}
	return out, nil
}
