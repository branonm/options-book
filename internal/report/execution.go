package report

import (
	"fmt"
	"io"

	"github.com/branonm/options-book/internal/broker"
	"github.com/branonm/options-book/internal/config"
	"github.com/branonm/options-book/internal/strategy/wheel"
)

// Execution is the ticket's execution section (PLAN.md addendum B): for each
// mode the account permits, how the order gets placed. It is appended to an
// accepted ticket; a rejected one offers nothing to place. The server never
// places an order in any mode.
type Execution struct {
	Modes []ModeSection `json:"modes"`
	// Ability says what the tools could not confirm about the account.
	Ability string `json:"ability"`
}

// ModeSection is one permitted mode.
type ModeSection struct {
	Mode string `json:"mode"`
	// Agentic: the exact order arguments, opener first, then the closer.
	Arguments []OrderArgs `json:"arguments,omitempty"`
	// Browser and manual: an entry checklist per order, in the form's order.
	Entries []Entry `json:"entries,omitempty"`
	// FormSource says whose field order the checklist follows.
	FormSource string `json:"form_source,omitempty"`
}

// OrderArgs is one order as the broker's connector takes it.
type OrderArgs struct {
	Role        string `json:"role"` // "opener" or "closer (Rule 10)"
	Account     string `json:"account"`
	Contract    string `json:"contract"`
	OptionID    string `json:"option_id,omitempty"`
	Side        string `json:"side"`
	PositionEff string `json:"position_effect"`
	Qty         int64  `json:"qty"`
	Type        string `json:"type"`
	Limit       string `json:"limit_price"`
	TimeInForce string `json:"time_in_force"`
}

// Entry is one order's checklist.
type Entry struct {
	Role   string              `json:"role"`
	Fields []broker.EntryField `json:"fields"`
}

const abilityNote = "not verified: get_accounts is not stored in a snapshot, so the option level and cash or margin that make this order possible were not checked. Confirm them in the account before entry."

// BuildExecution lists the permitted modes for an accepted ticket. It is nil
// for a rejected ticket or an account that is not in the book's config.
func BuildExecution(cfg *config.Book, t *wheel.Ticket) *Execution {
	if !t.Accepted {
		return nil
	}
	var acct *config.Account
	for i := range cfg.Accounts {
		if cfg.Accounts[i].Name == t.Account {
			acct = &cfg.Accounts[i]
		}
	}
	if acct == nil {
		return nil
	}
	orders := []struct {
		role string
		o    broker.EntryOrder
	}{{"opener", broker.EntryOrder{Side: "sell", Effect: "open", Symbol: t.Symbol, Right: rightOf(t.Kind),
		Strike: t.Strike.String(), Expiry: t.Expiry, Qty: t.Qty, Limit: t.Limit.StringFixed(2), TIF: "gfd"}}}
	if c := t.Closer; c != nil {
		o := orders[0].o
		o.Side, o.Effect, o.Qty, o.Limit, o.TIF = "buy", "close", c.Qty, c.Price.StringFixed(2), c.Tif
		orders = append(orders, struct {
			role string
			o    broker.EntryOrder
		}{"closer (Rule 10)", o})
	}
	ex := &Execution{Ability: abilityNote}
	form, haveForm := broker.LookupEntryForm(acct.Source)
	for _, m := range acct.Modes {
		s := ModeSection{Mode: m}
		switch m {
		case config.ModeAgentic:
			for _, x := range orders {
				a := OrderArgs{Role: x.role, Account: t.Account, Contract: t.Contract, Side: x.o.Side,
					PositionEff: x.o.Effect, Qty: x.o.Qty, Type: "limit", Limit: x.o.Limit, TimeInForce: x.o.TIF}
				if x.role == "opener" {
					a.OptionID = t.OptionID
				}
				s.Arguments = append(s.Arguments, a)
			}
		default:
			s.FormSource = acct.Source + " order form"
			if !haveForm {
				s.FormSource = "generic order (no " + acct.Source + " order form is known)"
			}
			for _, x := range orders {
				fields := genericFields(x.o)
				if haveForm {
					fields = form.EntryFields(x.o)
				}
				s.Entries = append(s.Entries, Entry{Role: x.role, Fields: fields})
			}
		}
		ex.Modes = append(ex.Modes, s)
	}
	return ex
}

func rightOf(kind string) string {
	if kind == wheel.TradeCoveredCall {
		return "call"
	}
	return "put"
}

func genericFields(o broker.EntryOrder) []broker.EntryField {
	return []broker.EntryField{
		{Name: "Contract", Value: fmt.Sprintf("%s %s $%s %s", o.Symbol, o.Expiry, o.Strike, o.Right)},
		{Name: "Action", Value: o.Side + " to " + o.Effect},
		{Name: "Order type", Value: "limit"},
		{Name: "Limit price", Value: "$" + o.Limit},
		{Name: "Quantity", Value: fmt.Sprint(o.Qty)},
		{Name: "Time in force", Value: o.TIF},
	}
}

func printExecution(out io.Writer, ex *Execution) {
	fmt.Fprintf(out, "\nExecution — the tools place nothing; the owner says yes to each order.\n  Ability: %s\n", ex.Ability)
	for _, m := range ex.Modes {
		fmt.Fprintf(out, "\n  [%s]\n", m.Mode)
		switch m.Mode {
		case config.ModeAgentic:
			fmt.Fprintln(out, "  Claude reviews each order through the connector, reads it back, and places on a yes.")
			for _, a := range m.Arguments {
				fmt.Fprintf(out, "    %s: account=%s contract=%s side=%s position_effect=%s qty=%d type=%s limit_price=%s time_in_force=%s\n",
					a.Role, a.Account, a.Contract, a.Side, a.PositionEff, a.Qty, a.Type, a.Limit, a.TimeInForce)
			}
		default:
			fmt.Fprintf(out, "  Entry checklist, field order of the %s:\n", m.FormSource)
			for _, e := range m.Entries {
				fmt.Fprintf(out, "    %s:\n", e.Role)
				for i, f := range e.Fields {
					fmt.Fprintf(out, "      %d. %s: %s\n", i+1, f.Name, f.Value)
				}
			}
		}
	}
}

// ticketJSON is the ticket with its execution section appended; the ticket's
// own fields keep their names and order.
type ticketJSON struct {
	*wheel.Ticket
	Execution *Execution `json:"execution,omitempty"`
}
