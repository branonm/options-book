// Package risk scores a proposed trade against rules.json's Rule 6 max-loss
// caps and, when a band is supplied, a delta band. It is broker-agnostic: the
// caller computes max loss and delta from whatever data it has (Robinhood,
// Alpaca, or a frozen snapshot) and passes in plain figures. This is
// deliberately separate from internal/strategy/wheel's Ticket: per rules.json,
// "caps_apply_to": "defined-risk structures only; wheel tickets state max
// loss in $ and % of pool without a cap" — the covered-call/put tickets
// Ticket builds are exempt by design, so this package exists for proposals
// Ticket doesn't cover (e.g. defined-risk spreads).
package risk

import (
	"fmt"

	"github.com/shopspring/decimal"

	"github.com/branonm/options-book/internal/rules"
)

// Proposal is a prospective trade's risk figures, already computed by the
// caller.
type Proposal struct {
	Symbol  string
	MaxLoss decimal.Decimal // dollars at risk if this proposal is filled

	// Delta and DeltaBand are both optional. Leave DeltaBand nil to skip the
	// band check entirely — not every structure in rules.json has one.
	Delta     decimal.Decimal
	DeltaBand *rules.Band
}

// Portfolio is the pool-level state the caps are measured against.
type Portfolio struct {
	PoolEquity decimal.Decimal

	// ExistingMaxLossUsed is the sum of max loss already committed by open
	// defined-risk structures in the pool, excluding this proposal.
	ExistingMaxLossUsed decimal.Decimal
}

// Result is a Proposal scored against r.MaxRisk (Rule 6) and, if
// p.DeltaBand was set, that band.
type Result struct {
	MaxLossPctPool    decimal.Decimal
	PerPositionCapPct decimal.Decimal

	// AggregateIfAcceptedPct is ExistingMaxLossUsed plus this proposal's max
	// loss, as a fraction of pool equity — the exposure the pool would carry
	// if this proposal fills.
	AggregateIfAcceptedPct decimal.Decimal
	AggregateCapPct        decimal.Decimal

	Rejections []string
	Warnings   []string
	Accepted   bool
}

func (res *Result) reject(f string, a ...any) {
	res.Rejections = append(res.Rejections, fmt.Sprintf(f, a...))
}
func (res *Result) warn(f string, a ...any) {
	res.Warnings = append(res.Warnings, fmt.Sprintf(f, a...))
}

// Check scores p against pf's pool equity and r.MaxRisk's caps, and against
// p.DeltaBand if set. It contacts no broker and reads no snapshot: p and pf
// must already reflect whatever the caller fetched.
func Check(r *rules.Rules, p Proposal, pf Portfolio) (*Result, error) {
	if !pf.PoolEquity.IsPositive() {
		return nil, fmt.Errorf("risk: pool equity must be > 0")
	}
	if p.MaxLoss.IsNegative() {
		return nil, fmt.Errorf("risk: max loss must be >= 0")
	}

	res := &Result{
		MaxLossPctPool:    p.MaxLoss.Div(pf.PoolEquity),
		PerPositionCapPct: r.MaxRisk.PerPositionCap,
		AggregateCapPct:   r.MaxRisk.AggregateCap,
		Rejections:        []string{},
		Warnings:          []string{},
	}
	res.AggregateIfAcceptedPct = pf.ExistingMaxLossUsed.Add(p.MaxLoss).Div(pf.PoolEquity)

	if res.MaxLossPctPool.GreaterThan(r.MaxRisk.PerPositionCap) {
		res.reject("%s max loss is %s%% of pool equity, over the %s%% per-position cap (Rule %d)",
			p.Symbol, pct(res.MaxLossPctPool), pct(r.MaxRisk.PerPositionCap), r.MaxRisk.Rule)
	}
	if res.AggregateIfAcceptedPct.GreaterThan(r.MaxRisk.AggregateCap) {
		res.reject("adding %s brings aggregate defined-risk exposure to %s%% of pool equity, over the %s%% aggregate cap (Rule %d)",
			p.Symbol, pct(res.AggregateIfAcceptedPct), pct(r.MaxRisk.AggregateCap), r.MaxRisk.Rule)
	}

	if p.DeltaBand != nil {
		b := *p.DeltaBand
		switch {
		case p.Delta.GreaterThan(b.Max):
			res.reject("%s delta %s is above the %s–%s band (%s)", p.Symbol, p.Delta.StringFixed(3), b.Min, b.Max, b.Rule)
		case p.Delta.LessThan(b.Min):
			res.warn("%s delta %s is below the %s–%s band (%s)", p.Symbol, p.Delta.StringFixed(3), b.Min, b.Max, b.Rule)
		}
	}

	res.Accepted = len(res.Rejections) == 0
	return res, nil
}

func pct(d decimal.Decimal) string { return d.Shift(2).StringFixed(1) }
