// Package alpacapull reads from Alpaca's paper-trading REST API and its
// market-data API, for the Alpaca Fetcher (PLAN.md item 6, step 4). It is
// read-only by construction, the way internal/rhmcp is for Robinhood: the
// only request method it can send is GET, and only to paths on a compiled
// allowlist (the account, positions, orders, order lookups, latest stock
// trades and option snapshots). There is no function in this package that
// places, replaces or cancels an order, and a test fails the build if one
// is added. Bodies are returned exactly as Alpaca sent them.
package alpacapull

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Hosts. The trading host is paper only; the data host serves quotes.
const (
	PaperHost = "paper-api.alpaca.markets"
	DataHost  = "data.alpaca.markets"
)

// allowed is every path a pull may read, by host. Anything else is refused
// before a request is built.
var allowed = map[string][]*regexp.Regexp{
	PaperHost: {
		regexp.MustCompile(`^/v2/account$`),
		regexp.MustCompile(`^/v2/positions$`),
		regexp.MustCompile(`^/v2/orders$`),
		regexp.MustCompile(`^/v2/orders/[0-9A-Za-z-]{8,64}$`),
		regexp.MustCompile(`^/v2/options/contracts$`),
	},
	DataHost: {
		regexp.MustCompile(`^/v2/stocks/trades/latest$`),
		regexp.MustCompile(`^/v1beta1/options/snapshots$`),
	},
}

// Allowed reports whether a read of path on host is on the allowlist.
func Allowed(host, path string) bool {
	for _, re := range allowed[host] {
		if re.MatchString(path) {
			return true
		}
	}
	return false
}

// Client is a read-only Alpaca client. Authorize adds the credential (the
// alpaca-paper provider's Authorize, which pins the hosts); it is required.
type Client struct {
	HC        *http.Client
	Authorize func(ctx context.Context, req *http.Request) error
	Now       func() time.Time
}

// Response is one body, exactly as Alpaca sent it.
type Response struct {
	Host      string
	Path      string
	Query     url.Values
	Body      []byte
	FetchedAt time.Time
}

// Call names the request the way provenance shows it: GET host/path?query.
func (r *Response) Call() string { return "GET " + r.Path }

// Args is the query as a JSON-able map of single values.
func (r *Response) Args() map[string]any {
	m := map[string]any{}
	for k, v := range r.Query {
		m[k] = strings.Join(v, ",")
	}
	return m
}

const maxBody = 32 << 20

// Get reads one allowlisted path.
func (c *Client) Get(ctx context.Context, host, path string, q url.Values) (*Response, error) {
	if !Allowed(host, path) {
		return nil, fmt.Errorf("alpacapull: %s%s is not on the read-only allowlist", host, path)
	}
	if c.Authorize == nil {
		return nil, fmt.Errorf("alpacapull: no credential")
	}
	u := url.URL{Scheme: "https", Host: host, Path: path, RawQuery: q.Encode()}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if err := c.Authorize(ctx, req); err != nil {
		return nil, err
	}
	hc := c.HC
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	fetched := now().UTC()
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("alpacapull: GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("alpacapull: GET %s: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		snip := string(body)
		if len(snip) > 200 {
			snip = snip[:200]
		}
		return nil, fmt.Errorf("alpacapull: GET %s: HTTP %d: %s", path, resp.StatusCode, snip)
	}
	if len(body) > maxBody {
		return nil, fmt.Errorf("alpacapull: GET %s: body over %d bytes", path, maxBody)
	}
	return &Response{Host: host, Path: path, Query: q, Body: body, FetchedAt: fetched}, nil
}

// Account is GET /v2/account.
func (c *Client) Account(ctx context.Context) (*Response, error) {
	return c.Get(ctx, PaperHost, "/v2/account", nil)
}

// Positions is GET /v2/positions: equities and options together.
func (c *Client) Positions(ctx context.Context) (*Response, error) {
	return c.Get(ctx, PaperHost, "/v2/positions", nil)
}

// OrdersPageLimit is the page size an order pull asks for (Alpaca's maximum).
const OrdersPageLimit = 500

// Orders is one page of GET /v2/orders. status is all or open; after, when set,
// is an RFC3339 timestamp, and the page runs oldest first from it.
func (c *Client) Orders(ctx context.Context, status, after string) (*Response, error) {
	q := url.Values{"status": {status}, "nested": {"true"}, "limit": {fmt.Sprint(OrdersPageLimit)}, "direction": {"asc"}}
	if after != "" {
		q.Set("after", after)
	}
	return c.Get(ctx, PaperHost, "/v2/orders", q)
}

// Order is GET /v2/orders/{id}.
func (c *Client) Order(ctx context.Context, id string) (*Response, error) {
	return c.Get(ctx, PaperHost, "/v2/orders/"+id, nil)
}

// LatestTrades is GET /v2/stocks/trades/latest for the symbols.
func (c *Client) LatestTrades(ctx context.Context, symbols []string) (*Response, error) {
	return c.Get(ctx, DataHost, "/v2/stocks/trades/latest", url.Values{"symbols": {strings.Join(symbols, ",")}})
}

// OptionSnapshots is GET /v1beta1/options/snapshots for the OCC symbols.
func (c *Client) OptionSnapshots(ctx context.Context, symbols []string) (*Response, error) {
	return c.Get(ctx, DataHost, "/v1beta1/options/snapshots", url.Values{"symbols": {strings.Join(symbols, ",")}})
}

// OptionContracts is GET /v2/options/contracts for the OCC symbols: the contract
// records, which carry open interest (the data API's snapshots do not).
func (c *Client) OptionContracts(ctx context.Context, symbols []string) (*Response, error) {
	return c.Get(ctx, PaperHost, "/v2/options/contracts", url.Values{"symbols": {strings.Join(symbols, ",")}})
}
