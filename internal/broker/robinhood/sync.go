package robinhood

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/branonm/options-book/internal/broker"
)

// Sync is the Robinhood order sync (broker.OrderSync): the shape of an order
// record's head, which states are final, how get_option_orders pages, and what
// the next incremental pull must ask for.
type Sync struct{}

func init() { broker.RegisterSync(Sync{}) }

func (Sync) Name() string { return Name }

// head is the part of a Robinhood order record the log compares.
type head struct {
	ID          string `json:"id"`
	State       string `json:"state"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
	Processed   string `json:"processed_quantity"`
	Canceled    string `json:"canceled_quantity"`
	Quantity    string `json:"quantity"`
	Price       string `json:"price"`
	TimeInForce string `json:"time_in_force"`
}

// Head reads a record's head. Version excludes updated_at: Robinhood touches
// every resting GTC order each morning without changing it, and counting that
// would log a change every day.
func (Sync) Head(record []byte) (broker.OrderHead, error) {
	var h head
	if err := json.Unmarshal(record, &h); err != nil {
		return broker.OrderHead{}, err
	}
	if h.ID == "" || h.State == "" || h.CreatedAt == "" {
		return broker.OrderHead{}, fmt.Errorf("order record missing id, state or created_at")
	}
	return broker.OrderHead{
		ID: h.ID, State: h.State, CreatedAt: h.CreatedAt, UpdatedAt: h.UpdatedAt,
		Version: strings.Join([]string{h.State, h.Processed, h.Canceled, h.Quantity, h.Price, h.TimeInForce}, "|"),
	}, nil
}

// Terminal states never change again.
func (Sync) Terminal(state string) bool {
	switch state {
	case "filled", "cancelled", "rejected", "failed", "voided":
		return true
	}
	return false
}

// Page reads one get_option_orders response.
func (Sync) Page(name string, body []byte) (broker.OrderPage, error) {
	var resp struct {
		Data struct {
			Orders []json.RawMessage `json:"orders"`
			Next   *string           `json:"next"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return broker.OrderPage{}, fmt.Errorf("%s: %w", name, err)
	}
	page := broker.OrderPage{Records: resp.Data.Orders}
	if resp.Data.Next != nil {
		page.Next = *resp.Data.Next
	}
	return page, nil
}

func (Sync) FullPull() string { return "get_option_orders (no filters, every page)" }

func (Sync) Lookup() string { return "get_option_orders(order_id=…)" }

// PullSteps: Robinhood's order query filters by creation date only, but an
// old order changes state (a closer created in October fills in November), so
// every order open in the log must be covered by the window, the open-order
// list, or a lookup by id.
func (s Sync) PullSteps(since string, openIDs []string) []string {
	out := []string{
		fmt.Sprintf("  1. get_option_orders(created_at_gte=%s), every page", since),
		"  2. get_option_orders(state=confirmed), plus queued and partially_filled if any",
	}
	if len(openIDs) > 0 {
		out = append(out, fmt.Sprintf("  3. every order open in the log must appear in 1 or 2; fetch any that don't with %s:", s.Lookup()))
		for _, id := range openIDs {
			out = append(out, "       "+id)
		}
	}
	return out
}
