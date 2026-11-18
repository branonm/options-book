package alpaca

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/branonm/options-book/internal/broker"
)

// PageLimit is the page size an order pull asks for (Alpaca's maximum). A page
// of exactly this many orders may have a successor, so Page reports a cursor
// for it and the log refuses the pull as incomplete.
const PageLimit = 500

// Sync is the Alpaca order sync (broker.OrderSync).
type Sync struct{}

func init() { broker.RegisterSync(Sync{}) }

func (Sync) Name() string { return Name }

// Head reads a record's head. Version covers the fields an order can change
// (status, fill, quantity, price, time in force) and not updated_at.
func (Sync) Head(record []byte) (broker.OrderHead, error) {
	var o rawOrder
	if err := json.Unmarshal(record, &o); err != nil {
		return broker.OrderHead{}, err
	}
	if o.ID == "" || o.Status == "" || o.CreatedAt == "" {
		return broker.OrderHead{}, fmt.Errorf("order record missing id, status or created_at")
	}
	return broker.OrderHead{ID: o.ID, State: o.Status, CreatedAt: o.CreatedAt, UpdatedAt: o.UpdatedAt,
		Version: strings.Join([]string{o.Status, o.FilledQty.text, o.FilledAvgPrice.text, o.Qty.text, o.LimitPrice.text, o.TimeInForce}, "|")}, nil
}

// Terminal states never change again.
func (Sync) Terminal(state string) bool {
	switch state {
	case "filled", "canceled", "expired", "replaced", "rejected":
		return true
	}
	return false
}

// Page reads one GET /v2/orders response, a JSON array, or the single order
// object GET /v2/orders/{id} returns (a one-record page). Alpaca pages by
// timestamp, not by cursor, so a full page is reported as having a successor.
func (Sync) Page(name string, body []byte) (broker.OrderPage, error) {
	var recs []json.RawMessage
	if t := strings.TrimSpace(string(body)); strings.HasPrefix(t, "{") {
		recs = []json.RawMessage{json.RawMessage(t)} // GET /v2/orders/{id} answers with the order itself
	} else if err := json.Unmarshal(body, &recs); err != nil {
		return broker.OrderPage{}, fmt.Errorf("%s: %w", name, err)
	}
	full := len(recs) >= PageLimit
	page := broker.OrderPage{}
	for _, r := range recs {
		var c struct {
			AssetClass string `json:"asset_class"`
		}
		if err := json.Unmarshal(r, &c); err != nil {
			return broker.OrderPage{}, fmt.Errorf("%s: %w", name, err)
		}
		if c.AssetClass == "" || c.AssetClass == "us_option" { // an equity order is not in an option order log
			page.Records = append(page.Records, r)
		}
	}
	if full {
		var last struct {
			CreatedAt string `json:"created_at"`
		}
		if err := json.Unmarshal(recs[len(recs)-1], &last); err != nil || last.CreatedAt == "" {
			return broker.OrderPage{}, fmt.Errorf("%s: a full page whose last order has no created_at", name)
		}
		page.Next = last.CreatedAt
	}
	return page, nil
}

func (Sync) FullPull() string {
	return fmt.Sprintf("GET /v2/orders?status=all&direction=asc&nested=true&limit=%d, then again with after=<last created_at> until a page has fewer than %d orders", PageLimit, PageLimit)
}

func (Sync) Lookup() string { return "GET /v2/orders/{order_id}?nested=true" }

// PullSteps: Alpaca's order list filters by time, but an old order changes
// state, so every order open in the log must be covered by the window, the
// open-order list, or a lookup by id.
func (s Sync) PullSteps(since string, openIDs []string) []string {
	out := []string{
		fmt.Sprintf("  1. GET /v2/orders?status=all&direction=asc&nested=true&limit=%d&after=%s, every page (repeat with after=<last created_at> while a page is full)", PageLimit, since),
		"  2. GET /v2/orders?status=open&nested=true&limit=500",
	}
	if len(openIDs) > 0 {
		out = append(out, fmt.Sprintf("  3. every order open in the log must appear in 1 or 2; fetch any that don't with %s:", s.Lookup()))
		for _, id := range openIDs {
			out = append(out, "       "+id)
		}
	}
	return out
}
