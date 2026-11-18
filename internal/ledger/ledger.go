// Package ledger turns raw order history and positions into the premium
// ledger: legs, realized P&L, open legs, and per-block breakevens (Rule 11).
// It is a pure function of its inputs.
//
// All money is shopspring decimal.Decimal. Compare with Equal/Cmp, never ==.
// Dollar totals read from Robinhood must be whole cents; anything else is an
// error rather than a rounding.
package ledger

import (
	"fmt"
	"sort"
	"strings"

	"github.com/shopspring/decimal"

	"github.com/branonm/options-book/internal/broker"
)

// DivPlaces is the precision kept for per-share quotients (fills, breakevens)
// before display rounding.
const DivPlaces = 10

// ContractKey identifies one option contract.
type ContractKey struct {
	Symbol string `json:"symbol"`
	Type   string `json:"type"` // call | put
	Strike string `json:"strike"`
	Expiry string `json:"expiry"`
}

func (k ContractKey) String() string {
	return fmt.Sprintf("%s $%s%s %s", k.Symbol, k.Strike, k.Type[:1], k.Expiry)
}

// Leg kinds.
const (
	KindCoveredCall    = "covered_call"
	KindShortPut       = "short_put"
	KindAcquisitionPut = "acquisition_put" // credit lives in share basis, not premium
)

// How a leg ended.
const (
	EndOpen    = "open"
	EndBought  = "bought_back"
	EndExpired = "expired_or_assigned" // left open through expiration, outcome not yet recorded
	// Resolved from the underlying's close on the expiration date (history/expirations.json).
	EndAssigned  = "assigned"          // finished in the money: shares called away (call) or bought (put)
	EndWorthless = "expired_worthless" // finished out of the money
)

// Fill is one order's contribution to a contract.
type Fill struct {
	OrderID  string          `json:"order_id"`
	OptionID string          `json:"-"`
	Time     string          `json:"time"`
	Effect   string          `json:"effect"` // open | close
	Qty      int64           `json:"qty"`
	Cash     decimal.Decimal `json:"cash"` // +credit received, -debit paid
	Tif      string          `json:"tif"`
}

// Leg is one position episode on a contract: from flat to flat (or still open).
type Leg struct {
	Account  string          `json:"account"`
	Key      ContractKey     `json:"contract"`
	OptionID string          `json:"option_id"`
	Kind     string          `json:"kind"`
	OpenedAt string          `json:"opened_at"`
	ClosedAt string          `json:"closed_at,omitempty"`
	End      string          `json:"end"`
	Qty      int64           `json:"qty"` // max contracts open during the episode
	OpenQty  int64           `json:"open_qty"`
	Credit   decimal.Decimal `json:"credit"`
	Debit    decimal.Decimal `json:"debit"`
	Net      decimal.Decimal `json:"net"`
	// FillPerShare is the blended opening credit per share:
	// total opening credit / (contracts opened × 100).
	FillPerShare decimal.Decimal `json:"fill_per_share"`
	Fills        []Fill          `json:"fills"`
	PreCampaign  bool            `json:"pre_campaign"`
	Note         string          `json:"note,omitempty"`
}

// Block is the (account, symbol) share holding the wheel writes against.
type Block struct {
	Account   string          `json:"account"`
	Symbol    string          `json:"symbol"`
	Shares    int64           `json:"shares"`
	Basis     decimal.Decimal `json:"basis_per_share"`
	Premium   decimal.Decimal `json:"premium"` // net premium on the block, including open legs' credit
	Breakeven decimal.Decimal `json:"breakeven"`
	Legs      int             `json:"legs"`
	Since     string          `json:"premium_since"`
}

// AccountLedger is the ledger for one account.
type AccountLedger struct {
	Account        string          `json:"account"`
	Realized       decimal.Decimal `json:"realized"`
	RealizedLegs   int             `json:"realized_legs"`
	ProfitableLegs int             `json:"profitable_legs"`
	OpenCredit     decimal.Decimal `json:"open_credit"`
	BasisCredit    decimal.Decimal `json:"acquisition_put_credit_in_basis"`
	PreCampaign    int             `json:"pre_campaign_legs_excluded"`
	// AssignedCallPremium is the net premium of calls that were assigned. Robinhood
	// books it into the share sale, not option P&L; reconciliation needs it.
	AssignedCallPremium decimal.Decimal `json:"assigned_call_premium"`
	Legs                []Leg           `json:"legs"`
	Blocks              []Block         `json:"blocks"`
	Warnings            []string        `json:"warnings,omitempty"`
}

// Input is everything one account's ledger is a function of.
type Input struct {
	Account       string
	AsOf          string            // YYYY-MM-DD; contracts expiring before this date are treated as ended
	CampaignStart string            // legs opened before this are excluded from totals
	BlockStarts   map[string]string // symbol -> date; premium counted from this date (default CampaignStart)
	Orders        []broker.Order
	Equity        []broker.EquityPosition
	// Expirations records how contracts that ran through expiration finished,
	// keyed by ContractKey.String(). Optional; unresolved legs are flagged.
	Expirations map[string]Expiration
}

// Build computes the ledger.
func Build(in Input) (*AccountLedger, error) {
	out := &AccountLedger{Account: in.Account}

	fills := map[ContractKey][]Fill{}
	for _, o := range in.Orders {
		if o.State != broker.StateFilled {
			continue
		}
		if len(o.Legs) != 1 {
			return nil, fmt.Errorf("order %s (%s): %d legs; multi-leg orders are not handled by the wheel ledger", o.ID, o.Symbol, len(o.Legs))
		}
		f, key, err := fillFromOrder(o, o.Legs[0])
		if err != nil {
			return nil, err
		}
		fills[key] = append(fills[key], f)
	}

	held := map[string]int64{}
	basis := map[string]decimal.Decimal{}
	for _, p := range in.Equity {
		if p.Side != "" && p.Side != broker.Long {
			return nil, fmt.Errorf("equity %s: unsupported position type %q", p.Symbol, p.Side)
		}
		q, err := parse(p.Quantity.Text)
		if err != nil || !q.IsInteger() {
			return nil, fmt.Errorf("equity %s: quantity %q is not whole shares", p.Symbol, p.Quantity.Text)
		}
		b, err := parse(p.AverageCost.Text)
		if err != nil {
			return nil, fmt.Errorf("equity %s: %w", p.Symbol, err)
		}
		held[p.Symbol] = q.IntPart()
		basis[p.Symbol] = b
	}

	keys := make([]ContractKey, 0, len(fills))
	for k := range fills {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })

	for _, k := range keys {
		fs := fills[k]
		sort.SliceStable(fs, func(i, j int) bool {
			if fs[i].Time != fs[j].Time {
				return fs[i].Time < fs[j].Time
			}
			return fs[i].Effect == "open" && fs[j].Effect == "close" // opens first on ties
		})
		var cur *Leg
		for _, f := range fs {
			switch f.Effect {
			case "open":
				if cur == nil {
					cur = &Leg{Account: in.Account, Key: k, OptionID: f.OptionID, OpenedAt: f.Time[:10]}
				}
				cur.OpenQty += f.Qty
				cur.Credit = cur.Credit.Add(f.Cash)
				if cur.OpenQty > cur.Qty {
					cur.Qty = cur.OpenQty
				}
			case "close":
				if cur == nil || f.Qty > cur.OpenQty {
					return nil, fmt.Errorf("%s: closing order %s buys %d contracts but only %d are open", k, f.OrderID, f.Qty, openQty(cur))
				}
				cur.OpenQty -= f.Qty
				cur.Debit = cur.Debit.Sub(f.Cash)
			}
			cur.Fills = append(cur.Fills, f)
			if cur.OpenQty == 0 {
				cur.End = EndBought
				cur.ClosedAt = f.Time[:10]
				out.Legs = append(out.Legs, *cur)
				cur = nil
			}
		}
		if cur != nil {
			if k.Expiry < in.AsOf {
				cur.End = EndExpired
				cur.ClosedAt = k.Expiry
				cur.Note = "no closing order; ended at expiration (expired worthless or assigned)"
			} else {
				cur.End = EndOpen
			}
			out.Legs = append(out.Legs, *cur)
		}
	}

	for i := range out.Legs {
		l := &out.Legs[i]
		l.Net = l.Credit.Sub(l.Debit)
		l.PreCampaign = l.OpenedAt < in.CampaignStart
		opened := int64(0)
		for _, f := range l.Fills {
			if f.Effect == "open" {
				opened += f.Qty
			}
		}
		l.FillPerShare = l.Credit.DivRound(decimal.NewFromInt(opened*100), DivPlaces)
		if l.End == EndExpired {
			resolveExpiration(l, in.Expirations)
		}
		switch {
		case l.Key.Type == "call":
			l.Kind = KindCoveredCall
		case l.End == EndAssigned:
			// An assigned put bought the shares: its credit is in Robinhood's
			// average cost, so it is basis, not premium (Rule 4a). This is
			// fixed at expiry and does not depend on what is held later.
			l.Kind = KindAcquisitionPut
		case l.End == EndExpired && held[l.Key.Symbol] > 0:
			// Outcome not recorded yet: provisional guess, flagged below.
			l.Kind = KindAcquisitionPut
		default:
			l.Kind = KindShortPut
		}
	}

	// Totals.
	for _, l := range out.Legs {
		if l.PreCampaign {
			out.PreCampaign++
			continue
		}
		switch {
		case l.Kind == KindAcquisitionPut:
			out.BasisCredit = out.BasisCredit.Add(l.Credit)
		case l.End == EndOpen:
			out.OpenCredit = out.OpenCredit.Add(l.Net)
		default:
			out.Realized = out.Realized.Add(l.Net)
			out.RealizedLegs++
			if l.Net.IsPositive() {
				out.ProfitableLegs++
			}
		}
		if l.End == EndExpired {
			out.Warnings = append(out.Warnings, fmt.Sprintf("%s: expiration outcome not recorded; provisionally counted as %s. Record it with `ob expirations`", l.Key, l.Kind))
		}
		if l.Key.Type == "call" && l.End == EndAssigned {
			out.AssignedCallPremium = out.AssignedCallPremium.Add(l.Net)
		}
	}

	// Blocks.
	syms := make([]string, 0, len(held))
	for s := range held {
		syms = append(syms, s)
	}
	sort.Strings(syms)
	for _, s := range syms {
		since := in.CampaignStart
		if d, ok := in.BlockStarts[s]; ok {
			since = d
		} else if r, warn := callAwayReset(out.Legs, s); warn != "" {
			out.Warnings = append(out.Warnings, warn)
		} else if r > since {
			// The shares were called away; premium before that belongs to the
			// old block, not this one.
			since = r
		}
		b := Block{Account: in.Account, Symbol: s, Shares: held[s], Basis: basis[s], Since: since}
		for _, l := range out.Legs {
			if l.Key.Symbol != s || l.Kind == KindAcquisitionPut || l.PreCampaign || l.OpenedAt < since {
				continue
			}
			b.Premium = b.Premium.Add(l.Net) // open legs: credit minus any partial buy-backs
			b.Legs++
		}
		// Rule 11: breakeven = (shares × basis − premium) ÷ shares
		shares := decimal.NewFromInt(b.Shares)
		b.Breakeven = shares.Mul(b.Basis).Sub(b.Premium).DivRound(shares, DivPlaces)
		out.Blocks = append(out.Blocks, b)
	}

	return out, nil
}

func openQty(l *Leg) int64 {
	if l == nil {
		return 0
	}
	return l.OpenQty
}

func parse(s string) (decimal.Decimal, error) {
	d, err := decimal.NewFromString(strings.TrimSpace(s))
	if err != nil {
		return decimal.Zero, fmt.Errorf("invalid decimal %q: %w", s, err)
	}
	return d, nil
}

// wholeCents rejects any dollar amount that is not an exact number of cents.
func wholeCents(d decimal.Decimal) error {
	if !d.Shift(2).IsInteger() {
		return fmt.Errorf("$%s is not a whole number of cents", d)
	}
	return nil
}

func fillFromOrder(o broker.Order, leg broker.Leg) (Fill, ContractKey, error) {
	strike, err := parse(leg.Strike.Text)
	if err != nil {
		return Fill{}, ContractKey{}, fmt.Errorf("order %s: strike: %w", o.ID, err)
	}
	key := ContractKey{Symbol: o.Symbol, Type: leg.Right, Strike: strike.String(), Expiry: leg.Expiry}
	mult, err := parse(o.Multiplier.Text)
	if err != nil {
		return Fill{}, key, fmt.Errorf("order %s: multiplier: %w", o.ID, err)
	}
	var effect string
	var sign decimal.Decimal
	switch {
	case leg.Side == broker.Sell && leg.Effect == broker.Open:
		effect, sign = "open", decimal.NewFromInt(1)
	case leg.Side == broker.Buy && leg.Effect == broker.Close:
		effect, sign = "close", decimal.NewFromInt(-1)
	default:
		return Fill{}, key, fmt.Errorf("order %s (%s): %s-to-%s is not a short-premium wheel leg", o.ID, key, leg.Side, leg.Effect)
	}
	if len(leg.Executions) == 0 {
		return Fill{}, key, fmt.Errorf("order %s (%s): filled but has no executions", o.ID, key)
	}
	total, qty := decimal.Zero, decimal.Zero
	first := leg.Executions[0].Time
	for _, e := range leg.Executions {
		p, err := parse(e.Price.Text)
		if err != nil {
			return Fill{}, key, fmt.Errorf("order %s: execution price: %w", o.ID, err)
		}
		q, err := parse(e.Quantity.Text)
		if err != nil {
			return Fill{}, key, fmt.Errorf("order %s: execution qty: %w", o.ID, err)
		}
		total = total.Add(p.Mul(q).Mul(mult))
		qty = qty.Add(q)
		if e.Time < first {
			first = e.Time
		}
	}
	if err := wholeCents(total); err != nil {
		return Fill{}, key, fmt.Errorf("order %s (%s): executions total %w", o.ID, key, err)
	}
	// The executions must agree with the order's own processed totals.
	pp, err := parse(o.Premium.Text)
	if err != nil {
		return Fill{}, key, fmt.Errorf("order %s (%s): processed_premium: %w", o.ID, key, err)
	}
	if !pp.Equal(total) {
		return Fill{}, key, fmt.Errorf("order %s (%s): executions sum to $%s but processed_premium is $%s", o.ID, key, total.StringFixed(2), pp.StringFixed(2))
	}
	pq, err := parse(o.FilledQuantity.Text)
	if err != nil || !pq.Equal(qty) || !qty.IsInteger() {
		return Fill{}, key, fmt.Errorf("order %s (%s): executed quantity %s does not match processed_quantity %s", o.ID, key, qty, o.FilledQuantity.Text)
	}
	return Fill{OrderID: o.ID, OptionID: leg.InstrumentID, Time: first, Effect: effect, Qty: qty.IntPart(), Cash: sign.Mul(total), Tif: o.TimeInForce}, key, nil
}
