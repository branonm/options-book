package robinhood

import (
	"fmt"

	"github.com/branonm/options-book/internal/broker"
)

// Form is Robinhood's option order form (broker.EntryForm). The field order
// is the web ticket's, top to bottom; confirm it against the live form the
// first time it is used, since nothing in the repo can check a web page.
type Form struct{}

func init() { broker.RegisterEntryForm(Form{}) }

func (Form) Name() string { return Name }

func (Form) EntryFields(o broker.EntryOrder) []broker.EntryField {
	tif := map[string]string{"gfd": "Good for day", "gtc": "Good till canceled"}[o.TIF]
	if tif == "" {
		tif = o.TIF
	}
	action := map[string]string{"sell/open": "Sell to open", "buy/close": "Buy to close"}[o.Side+"/"+o.Effect]
	if action == "" {
		action = o.Side + " to " + o.Effect
	}
	return []broker.EntryField{
		{Name: "Symbol", Value: o.Symbol},
		{Name: "Contract", Value: fmt.Sprintf("%s %s $%s %s", o.Expiry, o.Right, o.Strike, o.Symbol)},
		{Name: "Action", Value: action},
		{Name: "Order type", Value: "Limit"},
		{Name: "Limit price", Value: "$" + o.Limit},
		{Name: "Quantity", Value: fmt.Sprint(o.Qty)},
		{Name: "Time in force", Value: tif},
	}
}
