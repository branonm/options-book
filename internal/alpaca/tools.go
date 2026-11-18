package alpaca

import (
	"context"
	"fmt"
	"sort"
	"time"

	"cloud.google.com/go/civil"
	"github.com/alpacahq/alpaca-trade-api-go/v3/alpaca"
	"github.com/alpacahq/alpaca-trade-api-go/v3/marketdata"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shopspring/decimal"
)

// RegisterTools adds the paper-trading tools to server. Money and quantity
// fields are decimal strings, not floats, matching how the rest of this repo
// handles figures that must stay exact.
//
// StageOrder is the only tool here that changes state, and it still submits
// to Alpaca's paper account for real — nothing between the agent and the
// paper order is a human-approval gate yet (that's the agent backend's job,
// not this client's). Anything that wires this into an agent must put a
// confirmation step in front of it.
func RegisterTools(server *mcp.Server, client *Client) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "alpaca_get_positions",
		Description: "List the paper account's open equity and option positions.",
	}, client.getPositionsTool)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "alpaca_get_option_chain",
		Description: "Get the option chain snapshot (quotes, last trade, Greeks, IV) for an underlying symbol.",
	}, client.getOptionChainTool)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "alpaca_get_equity_quote",
		Description: "Get the latest NBBO quote for an equity symbol.",
	}, client.getEquityQuoteTool)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "alpaca_stage_order",
		Description: "Submit an order to the Alpaca PAPER trading account. Never touches a live account.",
	}, client.stageOrderTool)
}

type GetPositionsInput struct{}

type Position struct {
	Symbol        string `json:"symbol" jsonschema:"the position's symbol"`
	AssetClass    string `json:"asset_class" jsonschema:"us_equity or us_option"`
	Side          string `json:"side" jsonschema:"long or short"`
	Qty           string `json:"qty" jsonschema:"quantity held, as a decimal string"`
	AvgEntryPrice string `json:"avg_entry_price" jsonschema:"average entry price per unit, decimal string"`
	MarketValue   string `json:"market_value,omitempty" jsonschema:"current market value, decimal string"`
	UnrealizedPL  string `json:"unrealized_pl,omitempty" jsonschema:"unrealized profit/loss, decimal string"`
}

type GetPositionsOutput struct {
	Positions []Position `json:"positions"`
}

func (c *Client) getPositionsTool(ctx context.Context, req *mcp.CallToolRequest, in GetPositionsInput) (*mcp.CallToolResult, GetPositionsOutput, error) {
	positions, err := c.Positions()
	if err != nil {
		return nil, GetPositionsOutput{}, err
	}
	out := GetPositionsOutput{Positions: make([]Position, len(positions))}
	for i, p := range positions {
		out.Positions[i] = toPositionOutput(p)
	}
	return nil, out, nil
}

func toPositionOutput(p alpaca.Position) Position {
	out := Position{
		Symbol:        p.Symbol,
		AssetClass:    string(p.AssetClass),
		Side:          p.Side,
		Qty:           p.Qty.String(),
		AvgEntryPrice: p.AvgEntryPrice.String(),
	}
	if p.MarketValue != nil {
		out.MarketValue = p.MarketValue.String()
	}
	if p.UnrealizedPL != nil {
		out.UnrealizedPL = p.UnrealizedPL.String()
	}
	return out
}

type GetOptionChainInput struct {
	UnderlyingSymbol string `json:"underlying_symbol" jsonschema:"underlying equity symbol, e.g. AAPL"`
	ExpirationDate   string `json:"expiration_date,omitempty" jsonschema:"filter to one expiration date, YYYY-MM-DD"`
	OptionType       string `json:"option_type,omitempty" jsonschema:"call or put"`
}

type OptionLeg struct {
	Symbol            string  `json:"symbol" jsonschema:"the OCC option symbol"`
	BidPrice          float64 `json:"bid_price,omitempty"`
	AskPrice          float64 `json:"ask_price,omitempty"`
	LastTradePrice    float64 `json:"last_trade_price,omitempty"`
	ImpliedVolatility float64 `json:"implied_volatility,omitempty"`
	Delta             float64 `json:"delta,omitempty"`
	Gamma             float64 `json:"gamma,omitempty"`
	Theta             float64 `json:"theta,omitempty"`
	Vega              float64 `json:"vega,omitempty"`
	Rho               float64 `json:"rho,omitempty"`
}

type GetOptionChainOutput struct {
	Contracts []OptionLeg `json:"contracts"`
}

func (c *Client) getOptionChainTool(ctx context.Context, req *mcp.CallToolRequest, in GetOptionChainInput) (*mcp.CallToolResult, GetOptionChainOutput, error) {
	chainReq, err := parseOptionChainInput(in)
	if err != nil {
		return nil, GetOptionChainOutput{}, err
	}
	chain, err := c.OptionChain(in.UnderlyingSymbol, chainReq)
	if err != nil {
		return nil, GetOptionChainOutput{}, err
	}
	return nil, GetOptionChainOutput{Contracts: toOptionChainOutput(chain)}, nil
}

func parseOptionChainInput(in GetOptionChainInput) (marketdata.GetOptionChainRequest, error) {
	req := marketdata.GetOptionChainRequest{}
	if in.ExpirationDate != "" {
		d, err := civil.ParseDate(in.ExpirationDate)
		if err != nil {
			return req, fmt.Errorf("alpaca: invalid expiration_date %q: %w", in.ExpirationDate, err)
		}
		req.ExpirationDate = d
	}
	switch in.OptionType {
	case "":
	case string(marketdata.Call), string(marketdata.Put):
		req.Type = in.OptionType
	default:
		return req, fmt.Errorf("alpaca: invalid option_type %q: must be call or put", in.OptionType)
	}
	return req, nil
}

func toOptionChainOutput(chain map[string]marketdata.OptionSnapshot) []OptionLeg {
	legs := make([]OptionLeg, 0, len(chain))
	for symbol, snap := range chain {
		leg := OptionLeg{Symbol: symbol, ImpliedVolatility: snap.ImpliedVolatility}
		if snap.LatestQuote != nil {
			leg.BidPrice = snap.LatestQuote.BidPrice
			leg.AskPrice = snap.LatestQuote.AskPrice
		}
		if snap.LatestTrade != nil {
			leg.LastTradePrice = snap.LatestTrade.Price
		}
		if snap.Greeks != nil {
			leg.Delta = snap.Greeks.Delta
			leg.Gamma = snap.Greeks.Gamma
			leg.Theta = snap.Greeks.Theta
			leg.Vega = snap.Greeks.Vega
			leg.Rho = snap.Greeks.Rho
		}
		legs = append(legs, leg)
	}
	sort.Slice(legs, func(i, j int) bool { return legs[i].Symbol < legs[j].Symbol })
	return legs
}

type GetEquityQuoteInput struct {
	Symbol string `json:"symbol" jsonschema:"equity ticker symbol"`
}

type GetEquityQuoteOutput struct {
	Symbol    string  `json:"symbol"`
	BidPrice  float64 `json:"bid_price"`
	AskPrice  float64 `json:"ask_price"`
	Timestamp string  `json:"timestamp" jsonschema:"RFC3339 timestamp of the quote"`
}

func (c *Client) getEquityQuoteTool(ctx context.Context, req *mcp.CallToolRequest, in GetEquityQuoteInput) (*mcp.CallToolResult, GetEquityQuoteOutput, error) {
	q, err := c.EquityQuote(in.Symbol)
	if err != nil {
		return nil, GetEquityQuoteOutput{}, err
	}
	return nil, toEquityQuoteOutput(in.Symbol, q), nil
}

func toEquityQuoteOutput(symbol string, q *marketdata.Quote) GetEquityQuoteOutput {
	return GetEquityQuoteOutput{
		Symbol:    symbol,
		BidPrice:  q.BidPrice,
		AskPrice:  q.AskPrice,
		Timestamp: q.Timestamp.Format(time.RFC3339),
	}
}

type StageOrderInput struct {
	Symbol      string `json:"symbol" jsonschema:"the order's symbol: an equity ticker or an OCC option symbol"`
	Side        string `json:"side" jsonschema:"buy or sell"`
	Qty         string `json:"qty" jsonschema:"quantity, as a decimal string"`
	Type        string `json:"type" jsonschema:"market, limit, stop, stop_limit, or trailing_stop"`
	TimeInForce string `json:"time_in_force" jsonschema:"day, gtc, opg, cls, ioc, or fok"`
	LimitPrice  string `json:"limit_price,omitempty" jsonschema:"limit price, decimal string; required for limit orders"`
}

type StageOrderOutput struct {
	OrderID string `json:"order_id"`
	Status  string `json:"status"`
	Symbol  string `json:"symbol"`
	Side    string `json:"side"`
	Qty     string `json:"qty,omitempty"`
	Type    string `json:"type"`
}

func (c *Client) stageOrderTool(ctx context.Context, req *mcp.CallToolRequest, in StageOrderInput) (*mcp.CallToolResult, StageOrderOutput, error) {
	placeReq, err := parseStageOrderInput(in)
	if err != nil {
		return nil, StageOrderOutput{}, err
	}
	order, err := c.StageOrder(placeReq)
	if err != nil {
		return nil, StageOrderOutput{}, err
	}
	return nil, toStageOrderOutput(order), nil
}

func parseStageOrderInput(in StageOrderInput) (alpaca.PlaceOrderRequest, error) {
	qty, err := decimal.NewFromString(in.Qty)
	if err != nil {
		return alpaca.PlaceOrderRequest{}, fmt.Errorf("alpaca: invalid qty %q: %w", in.Qty, err)
	}
	req := alpaca.PlaceOrderRequest{
		Symbol:      in.Symbol,
		Qty:         &qty,
		Side:        alpaca.Side(in.Side),
		Type:        alpaca.OrderType(in.Type),
		TimeInForce: alpaca.TimeInForce(in.TimeInForce),
	}
	if in.LimitPrice != "" {
		lp, err := decimal.NewFromString(in.LimitPrice)
		if err != nil {
			return alpaca.PlaceOrderRequest{}, fmt.Errorf("alpaca: invalid limit_price %q: %w", in.LimitPrice, err)
		}
		req.LimitPrice = &lp
	}
	return req, nil
}

func toStageOrderOutput(o *alpaca.Order) StageOrderOutput {
	out := StageOrderOutput{
		OrderID: o.ID,
		Status:  o.Status,
		Symbol:  o.Symbol,
		Side:    string(o.Side),
		Type:    string(o.Type),
	}
	if o.Qty != nil {
		out.Qty = o.Qty.String()
	}
	return out
}
