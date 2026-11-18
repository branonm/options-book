package alpaca

import (
	"testing"
	"time"

	"github.com/alpacahq/alpaca-trade-api-go/v3/alpaca"
	"github.com/alpacahq/alpaca-trade-api-go/v3/marketdata"
	"github.com/shopspring/decimal"
)

func dec(s string) decimal.Decimal {
	d, err := decimal.NewFromString(s)
	if err != nil {
		panic(err)
	}
	return d
}

func TestToPositionOutput(t *testing.T) {
	mv := dec("1234.56")
	pl := dec("-10.00")
	p := alpaca.Position{
		Symbol:        "AAPL241220P00150000",
		AssetClass:    alpaca.AssetClass("us_option"),
		Side:          "short",
		Qty:           dec("-1"),
		AvgEntryPrice: dec("2.35"),
		MarketValue:   &mv,
		UnrealizedPL:  &pl,
	}
	out := toPositionOutput(p)
	if out.Symbol != p.Symbol || out.Side != "short" || out.Qty != "-1" || out.AvgEntryPrice != "2.35" {
		t.Fatalf("got %+v", out)
	}
	if out.MarketValue != "1234.56" || out.UnrealizedPL != "-10" {
		t.Fatalf("got %+v", out)
	}
}

func TestToPositionOutputNilOptionalFields(t *testing.T) {
	p := alpaca.Position{Symbol: "SPY", Side: "long", Qty: dec("100"), AvgEntryPrice: dec("450")}
	out := toPositionOutput(p)
	if out.MarketValue != "" || out.UnrealizedPL != "" {
		t.Fatalf("expected empty strings for nil fields, got %+v", out)
	}
}

func TestParseOptionChainInput(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		req, err := parseOptionChainInput(GetOptionChainInput{ExpirationDate: "2026-12-20", OptionType: "call"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if req.Type != "call" {
			t.Fatalf("got type %q", req.Type)
		}
		if req.ExpirationDate.Year != 2026 || req.ExpirationDate.Month != time.December || req.ExpirationDate.Day != 20 {
			t.Fatalf("got expiration %+v", req.ExpirationDate)
		}
	})

	t.Run("bad date", func(t *testing.T) {
		if _, err := parseOptionChainInput(GetOptionChainInput{ExpirationDate: "not-a-date"}); err == nil {
			t.Fatal("expected an error for an invalid date")
		}
	})

	t.Run("bad option type", func(t *testing.T) {
		if _, err := parseOptionChainInput(GetOptionChainInput{OptionType: "straddle"}); err == nil {
			t.Fatal("expected an error for an invalid option_type")
		}
	})
}

func TestToOptionChainOutputSortsBySymbol(t *testing.T) {
	chain := map[string]marketdata.OptionSnapshot{
		"B": {ImpliedVolatility: 0.2, Greeks: &marketdata.OptionGreeks{Delta: 0.5}},
		"A": {
			LatestQuote: &marketdata.OptionQuote{BidPrice: 1.1, AskPrice: 1.3},
			LatestTrade: &marketdata.OptionTrade{Price: 1.2},
		},
	}
	legs := toOptionChainOutput(chain)
	if len(legs) != 2 || legs[0].Symbol != "A" || legs[1].Symbol != "B" {
		t.Fatalf("got %+v", legs)
	}
	if legs[0].BidPrice != 1.1 || legs[0].AskPrice != 1.3 || legs[0].LastTradePrice != 1.2 {
		t.Fatalf("got %+v", legs[0])
	}
	if legs[1].Delta != 0.5 || legs[1].ImpliedVolatility != 0.2 {
		t.Fatalf("got %+v", legs[1])
	}
}

func TestToEquityQuoteOutput(t *testing.T) {
	ts := time.Date(2026, 11, 16, 14, 30, 0, 0, time.UTC)
	q := &marketdata.Quote{BidPrice: 100.1, AskPrice: 100.2, Timestamp: ts}
	out := toEquityQuoteOutput("AAPL", q)
	if out.Symbol != "AAPL" || out.BidPrice != 100.1 || out.AskPrice != 100.2 {
		t.Fatalf("got %+v", out)
	}
	if out.Timestamp != ts.Format(time.RFC3339) {
		t.Fatalf("got timestamp %q", out.Timestamp)
	}
}

func TestParseStageOrderInput(t *testing.T) {
	t.Run("market order", func(t *testing.T) {
		req, err := parseStageOrderInput(StageOrderInput{
			Symbol: "AAPL", Side: "buy", Qty: "10", Type: "market", TimeInForce: "day",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if req.Symbol != "AAPL" || req.Side != alpaca.Buy || req.Type != alpaca.Market || req.TimeInForce != alpaca.Day {
			t.Fatalf("got %+v", req)
		}
		if req.Qty == nil || !req.Qty.Equal(dec("10")) {
			t.Fatalf("got qty %v", req.Qty)
		}
		if req.LimitPrice != nil {
			t.Fatalf("expected no limit price, got %v", req.LimitPrice)
		}
	})

	t.Run("limit order", func(t *testing.T) {
		req, err := parseStageOrderInput(StageOrderInput{
			Symbol: "AAPL", Side: "sell", Qty: "1", Type: "limit", TimeInForce: "gtc", LimitPrice: "150.25",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if req.LimitPrice == nil || !req.LimitPrice.Equal(dec("150.25")) {
			t.Fatalf("got limit price %v", req.LimitPrice)
		}
	})

	t.Run("bad qty", func(t *testing.T) {
		if _, err := parseStageOrderInput(StageOrderInput{Qty: "ten"}); err == nil {
			t.Fatal("expected an error for a non-numeric qty")
		}
	})

	t.Run("bad limit price", func(t *testing.T) {
		if _, err := parseStageOrderInput(StageOrderInput{Qty: "1", LimitPrice: "cheap"}); err == nil {
			t.Fatal("expected an error for a non-numeric limit_price")
		}
	})
}

func TestToStageOrderOutput(t *testing.T) {
	qty := dec("5")
	o := &alpaca.Order{ID: "order-1", Status: "accepted", Symbol: "AAPL", Side: alpaca.Buy, Type: alpaca.Market, Qty: &qty}
	out := toStageOrderOutput(o)
	if out.OrderID != "order-1" || out.Status != "accepted" || out.Symbol != "AAPL" || out.Side != "buy" || out.Type != "market" || out.Qty != "5" {
		t.Fatalf("got %+v", out)
	}
}
