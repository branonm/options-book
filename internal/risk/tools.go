package risk

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shopspring/decimal"

	"github.com/branonm/options-book/internal/rules"
)

// RegisterTools adds the proposal risk-check tool to server. Figures are
// decimal strings, not floats, matching how the rest of this repo handles
// numbers that must stay exact. It contacts no broker: every input is
// supplied by the caller.
func RegisterTools(server *mcp.Server, r *rules.Rules) {
	s := &toolServer{rules: r}
	mcp.AddTool(server, &mcp.Tool{
		Name:        "risk_check_proposal",
		Description: "Score a proposed trade's max loss (and, optionally, delta) against rules.json's Rule 6 per-position and aggregate caps.",
	}, s.checkProposalTool)
}

type toolServer struct {
	rules *rules.Rules
}

type CheckProposalInput struct {
	Symbol              string `json:"symbol" jsonschema:"the proposal's symbol"`
	MaxLoss             string `json:"max_loss" jsonschema:"dollars at risk if this proposal is filled, decimal string"`
	PoolEquity          string `json:"pool_equity" jsonschema:"the pool's total equity, decimal string"`
	ExistingMaxLossUsed string `json:"existing_max_loss_used,omitempty" jsonschema:"sum of max loss already committed by open defined-risk structures in the pool, decimal string; defaults to 0"`
	Delta               string `json:"delta,omitempty" jsonschema:"absolute delta of the proposed leg, decimal string; omit to skip the delta-band check"`
	DeltaBand           string `json:"delta_band,omitempty" jsonschema:"which rules.json delta band to check delta against: covered_call, acquisition_put, or income_put; omit to skip"`
}

type CheckProposalOutput struct {
	MaxLossPctPool         string   `json:"max_loss_pct_pool"`
	PerPositionCapPct      string   `json:"per_position_cap_pct"`
	AggregateIfAcceptedPct string   `json:"aggregate_if_accepted_pct"`
	AggregateCapPct        string   `json:"aggregate_cap_pct"`
	Rejections             []string `json:"rejections"`
	Warnings               []string `json:"warnings"`
	Accepted               bool     `json:"accepted"`
}

func (s *toolServer) checkProposalTool(ctx context.Context, req *mcp.CallToolRequest, in CheckProposalInput) (*mcp.CallToolResult, CheckProposalOutput, error) {
	p, pf, err := parseCheckProposalInput(s.rules, in)
	if err != nil {
		return nil, CheckProposalOutput{}, err
	}
	res, err := Check(s.rules, p, pf)
	if err != nil {
		return nil, CheckProposalOutput{}, err
	}
	return nil, toCheckProposalOutput(res), nil
}

func parseCheckProposalInput(r *rules.Rules, in CheckProposalInput) (Proposal, Portfolio, error) {
	maxLoss, err := decimal.NewFromString(in.MaxLoss)
	if err != nil {
		return Proposal{}, Portfolio{}, fmt.Errorf("risk: invalid max_loss %q: %w", in.MaxLoss, err)
	}
	poolEquity, err := decimal.NewFromString(in.PoolEquity)
	if err != nil {
		return Proposal{}, Portfolio{}, fmt.Errorf("risk: invalid pool_equity %q: %w", in.PoolEquity, err)
	}
	existing := decimal.Zero
	if in.ExistingMaxLossUsed != "" {
		existing, err = decimal.NewFromString(in.ExistingMaxLossUsed)
		if err != nil {
			return Proposal{}, Portfolio{}, fmt.Errorf("risk: invalid existing_max_loss_used %q: %w", in.ExistingMaxLossUsed, err)
		}
	}

	p := Proposal{Symbol: in.Symbol, MaxLoss: maxLoss}
	if in.Delta != "" {
		d, err := decimal.NewFromString(in.Delta)
		if err != nil {
			return Proposal{}, Portfolio{}, fmt.Errorf("risk: invalid delta %q: %w", in.Delta, err)
		}
		p.Delta = d
	}
	if in.DeltaBand != "" {
		switch in.DeltaBand {
		case "covered_call":
			p.DeltaBand = &r.DeltaBands.CoveredCall
		case "acquisition_put":
			p.DeltaBand = &r.DeltaBands.AcquisitionPut
		case "income_put":
			p.DeltaBand = &r.DeltaBands.IncomePut
		default:
			return Proposal{}, Portfolio{}, fmt.Errorf("risk: invalid delta_band %q: must be covered_call, acquisition_put, or income_put", in.DeltaBand)
		}
	}

	return p, Portfolio{PoolEquity: poolEquity, ExistingMaxLossUsed: existing}, nil
}

func toCheckProposalOutput(res *Result) CheckProposalOutput {
	return CheckProposalOutput{
		MaxLossPctPool:         res.MaxLossPctPool.String(),
		PerPositionCapPct:      res.PerPositionCapPct.String(),
		AggregateIfAcceptedPct: res.AggregateIfAcceptedPct.String(),
		AggregateCapPct:        res.AggregateCapPct.String(),
		Rejections:             res.Rejections,
		Warnings:               res.Warnings,
		Accepted:               res.Accepted,
	}
}
