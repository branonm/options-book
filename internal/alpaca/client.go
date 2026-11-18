// Package alpaca wraps Alpaca's trading and market-data clients for the
// options agent's paper-trading account. Unlike internal/rh, which only
// decodes saved snapshots, this client calls Alpaca directly — paper trading
// only, per the project handoff doc's guardrails. It never points at Alpaca's
// live endpoint.
package alpaca

import (
	"errors"
	"os"

	"github.com/alpacahq/alpaca-trade-api-go/v3/alpaca"
	"github.com/alpacahq/alpaca-trade-api-go/v3/marketdata"
)

// paperBaseURL is Alpaca's paper-trading trading API. The SDK defaults to the
// live endpoint when BaseURL is left empty, so this is always set explicitly.
const paperBaseURL = "https://paper-api.alpaca.markets"

// Config holds the paper-trading API credentials.
type Config struct {
	APIKeyID     string
	APISecretKey string
}

// ConfigFromEnv reads APCA_API_KEY_ID and APCA_API_SECRET_KEY, Alpaca's own
// environment variable names.
func ConfigFromEnv() (Config, error) {
	cfg := Config{
		APIKeyID:     os.Getenv("APCA_API_KEY_ID"),
		APISecretKey: os.Getenv("APCA_API_SECRET_KEY"),
	}
	if cfg.APIKeyID == "" || cfg.APISecretKey == "" {
		return Config{}, errors.New("alpaca: APCA_API_KEY_ID and APCA_API_SECRET_KEY must both be set")
	}
	return cfg, nil
}

// Client is a thin wrapper over Alpaca's trading and market-data clients,
// pinned to the paper-trading endpoint.
type Client struct {
	trading *alpaca.Client
	data    *marketdata.Client
}

// NewClient builds a Client against Alpaca's paper-trading API.
func NewClient(cfg Config) (*Client, error) {
	if cfg.APIKeyID == "" || cfg.APISecretKey == "" {
		return nil, errors.New("alpaca: APIKeyID and APISecretKey are required")
	}
	return &Client{
		trading: alpaca.NewClient(alpaca.ClientOpts{
			APIKey:    cfg.APIKeyID,
			APISecret: cfg.APISecretKey,
			BaseURL:   paperBaseURL,
		}),
		data: marketdata.NewClient(marketdata.ClientOpts{
			APIKey:    cfg.APIKeyID,
			APISecret: cfg.APISecretKey,
		}),
	}, nil
}

// Positions returns the paper account's open positions (equities and options).
func (c *Client) Positions() ([]alpaca.Position, error) {
	return c.trading.GetPositions()
}

// OptionChain returns the snapshot chain (quotes, trades, Greeks, IV) for an
// underlying symbol.
func (c *Client) OptionChain(underlyingSymbol string, req marketdata.GetOptionChainRequest) (map[string]marketdata.OptionSnapshot, error) {
	return c.data.GetOptionChain(underlyingSymbol, req)
}

// EquityQuote returns the latest NBBO quote for an equity symbol.
func (c *Client) EquityQuote(symbol string) (*marketdata.Quote, error) {
	return c.data.GetLatestQuote(symbol, marketdata.GetLatestQuoteRequest{})
}

// StageOrder submits an order to the paper account. There is no separate
// preview endpoint on Alpaca's API; paper trading stands in for preview since
// nothing here executes against a real account. The human-approval gate that
// decides whether to call this at all is part of the agent backend, not this
// client.
func (c *Client) StageOrder(req alpaca.PlaceOrderRequest) (*alpaca.Order, error) {
	return c.trading.PlaceOrder(req)
}
