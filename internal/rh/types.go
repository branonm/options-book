// Package rh decodes raw Robinhood MCP responses exactly as saved to disk.
// Nothing here interprets the data; it only gives it Go types.
package rh

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
)

type Execution struct {
	ID        string `json:"id"`
	Price     string `json:"price"`
	Quantity  string `json:"quantity"`
	TradeDate string `json:"trade_date"`
	Timestamp string `json:"timestamp"`
}

type OrderLeg struct {
	ID             string      `json:"id"`
	OptionID       string      `json:"option_id"`
	Side           string      `json:"side"`            // buy | sell
	PositionEffect string      `json:"position_effect"` // open | close
	RatioQuantity  int         `json:"ratio_quantity"`
	ExpirationDate string      `json:"expiration_date"`
	StrikePrice    string      `json:"strike_price"`
	OptionType     string      `json:"option_type"` // call | put
	Executions     []Execution `json:"executions"`
}

type OptionOrder struct {
	ID                   string     `json:"id"`
	ChainSymbol          string     `json:"chain_symbol"`
	State                string     `json:"state"`
	Type                 string     `json:"type"`
	Direction            string     `json:"direction"`
	Quantity             string     `json:"quantity"`
	ProcessedQuantity    string     `json:"processed_quantity"`
	Price                string     `json:"price"`
	ProcessedPremium     string     `json:"processed_premium"`
	TradeValueMultiplier string     `json:"trade_value_multiplier"`
	TimeInForce          string     `json:"time_in_force"`
	OpeningStrategy      *string    `json:"opening_strategy"`
	ClosingStrategy      *string    `json:"closing_strategy"`
	CreatedAt            string     `json:"created_at"`
	Legs                 []OrderLeg `json:"legs"`
}

type OptionOrdersResponse struct {
	Data struct {
		Orders []OptionOrder `json:"orders"`
		Next   *string       `json:"next"`
	} `json:"data"`
}

type EquityPosition struct {
	Symbol          string `json:"symbol"`
	Quantity        string `json:"quantity"`
	AverageBuyPrice string `json:"average_buy_price"`
	Type            string `json:"type"`
}

type EquityPositionsResponse struct {
	Data struct {
		Positions []EquityPosition `json:"positions"`
		Next      *string          `json:"next"`
	} `json:"data"`
}

type EquityQuote struct {
	Symbol             string `json:"symbol"`
	LastTradePrice     string `json:"last_trade_price"`
	VenueLastTradeTime string `json:"venue_last_trade_time"`
	State              string `json:"state"`
}

type EquityQuotesResponse struct {
	Data struct {
		Results []struct {
			Quote EquityQuote `json:"quote"`
		} `json:"results"`
	} `json:"data"`
}

type OptionPosition struct {
	OptionID           string `json:"option_id"`
	ChainSymbol        string `json:"chain_symbol"`
	Type               string `json:"type"` // long | short
	Quantity           string `json:"quantity"`
	AveragePrice       string `json:"average_price"` // per contract, negative for credit
	ExpirationDate     string `json:"expiration_date"`
	PendingBuyQuantity string `json:"pending_buy_quantity"`
}

type OptionPositionsResponse struct {
	Data struct {
		Positions []OptionPosition `json:"positions"`
		Next      *string          `json:"next"`
	} `json:"data"`
}

// LoadJSON decodes a saved MCP response. It refuses paginated responses whose
// next cursor is set, because a partial history would silently undercount.
func LoadJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return DecodeJSON(path, b, v)
}

// LoadJSONFS is LoadJSON over a read-only Source view (fs.FS).
func LoadJSONFS(fsys fs.FS, name string, v any) error {
	b, err := fs.ReadFile(fsys, name)
	if err != nil {
		return err
	}
	return DecodeJSON(name, b, v)
}

// DecodeJSON is LoadJSON's check on bytes already read; path names them in errors.
func DecodeJSON(path string, b []byte, v any) error {
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	var probe struct {
		Data struct {
			Next *string `json:"next"`
		} `json:"data"`
	}
	_ = json.Unmarshal(b, &probe)
	if probe.Data.Next != nil && *probe.Data.Next != "" {
		return fmt.Errorf("%s: response is paginated (next cursor set); fetch every page first", path)
	}
	return nil
}

type Portfolio struct {
	TotalValue  string `json:"total_value"`
	EquityValue string `json:"equity_value"`
	Cash        string `json:"cash"`
	BuyingPower struct {
		BuyingPower string `json:"buying_power"`
	} `json:"buying_power"`
}

type PortfolioResponse struct {
	Data Portfolio `json:"data"`
}

type OptionQuote struct {
	InstrumentID      string `json:"instrument_id"`
	BidPrice          string `json:"bid_price"`
	AskPrice          string `json:"ask_price"`
	MarkPrice         string `json:"mark_price"`
	Delta             string `json:"delta"`
	ImpliedVolatility string `json:"implied_volatility"`
	OpenInterest      int64  `json:"open_interest"`
	Volume            int64  `json:"volume"`
	UpdatedAt         string `json:"updated_at"`
}

type OptionQuotesResponse struct {
	Data struct {
		Results []struct {
			Quote OptionQuote `json:"quote"`
		} `json:"results"`
	} `json:"data"`
}

type OptionInstrument struct {
	ID                   string `json:"id"`
	ChainSymbol          string `json:"chain_symbol"`
	ExpirationDate       string `json:"expiration_date"`
	StrikePrice          string `json:"strike_price"`
	Type                 string `json:"type"`
	State                string `json:"state"`
	Tradability          string `json:"tradability"`
	TradeValueMultiplier string `json:"trade_value_multiplier"`
	MinTicks             struct {
		AboveTick   string `json:"above_tick"`
		BelowTick   string `json:"below_tick"`
		CutoffPrice string `json:"cutoff_price"`
	} `json:"min_ticks"`
}

type OptionInstrumentsResponse struct {
	Data struct {
		Instruments []OptionInstrument `json:"instruments"`
		Next        *string            `json:"next"`
	} `json:"data"`
}

type EquityBar struct {
	BeginsAt     string `json:"begins_at"`
	ClosePrice   string `json:"close_price"`
	Interpolated bool   `json:"interpolated"`
	Session      string `json:"session"`
}

type EquityHistoricalsResponse struct {
	Data struct {
		Results []struct {
			Symbol   string      `json:"symbol"`
			Interval string      `json:"interval"`
			Bounds   string      `json:"bounds"`
			Bars     []EquityBar `json:"bars"`
		} `json:"results"`
	} `json:"data"`
}

type RealizedPnLResponse struct {
	Data struct {
		Window       string `json:"window"` // "YYYY-MM-DD..YYYY-MM-DD"
		TotalReturns string `json:"total_returns"`
	} `json:"data"`
}
