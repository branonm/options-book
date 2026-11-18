// Package rules loads config/rules.json: the single source of every numeric
// trading rule the tools enforce.
package rules

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"time"
	_ "time/tzdata" // embed zoneinfo so America/New_York resolves anywhere

	"github.com/shopspring/decimal"
)

type Rules struct {
	CoveredCallCloser struct {
		Rule           int             `json:"rule"`
		FractionOfFill decimal.Decimal `json:"fraction_of_fill"`
		TimeInForce    string          `json:"time_in_force"`
		Rounding       string          `json:"rounding"`
	} `json:"covered_call_closer"`
	OptionTicks struct {
		Cutoff          decimal.Decimal `json:"cutoff"`
		BelowCutoff     decimal.Decimal `json:"below_cutoff"`
		AtOrAboveCutoff decimal.Decimal `json:"at_or_above_cutoff"`
	} `json:"option_ticks"`
	AcquisitionPut struct {
		Rule          int  `json:"rule"`
		CloserAllowed bool `json:"closer_allowed"`
	} `json:"acquisition_put"`
	RollTrigger struct {
		Rule         int `json:"rule"`
		ITMAndMaxDTE int `json:"itm_and_max_dte"`
	} `json:"roll_trigger"`
	// ConcentrationCaps is optional: each entry caps the share of invested equity
	// that one group of symbols may hold, per pool and book-wide. No entries, no caps.
	ConcentrationCaps []ConcentrationCap `json:"concentration_caps"`
	LotSize           struct {
		SharesPerContract int64 `json:"shares_per_contract"`
	} `json:"lot_size"`
	DeltaBands struct {
		CoveredCall    Band `json:"covered_call"`
		AcquisitionPut Band `json:"acquisition_put"`
		IncomePut      Band `json:"income_put"`
	} `json:"delta_bands"`
	IncomePutCloser struct {
		Rule           string          `json:"rule"`
		FractionOfFill decimal.Decimal `json:"fraction_of_fill"`
		TimeInForce    string          `json:"time_in_force"`
	} `json:"income_put_closer"`
	GradedClearance struct {
		Rule            int             `json:"rule"`
		NearBasisDepth  decimal.Decimal `json:"near_basis_depth"`
		NearBasisMin    decimal.Decimal `json:"near_basis_min"`
		NearBasisTarget decimal.Decimal `json:"near_basis_target"`
		DeepDepth       decimal.Decimal `json:"deep_depth"`
		DeepMin         decimal.Decimal `json:"deep_min"`
	} `json:"graded_clearance"`
	MinOpenInterest struct {
		Rule  string `json:"rule"`
		Value int64  `json:"value"`
	} `json:"min_open_interest"`
	QuoteWindow struct {
		Rule                  string `json:"rule"`
		Timezone              string `json:"timezone"`
		Open                  string `json:"open"`
		Close                 string `json:"close"`
		ExcludeOpeningMinutes int    `json:"exclude_opening_minutes"`
	} `json:"quote_window"`
	Cooldown struct {
		Rule                  int             `json:"rule"`
		Losses                int             `json:"losses"`
		WithinSessions        int             `json:"within_sessions"`
		SingleLossShareOfPool decimal.Decimal `json:"single_loss_share_of_pool"`
		FreezeSessions        int             `json:"freeze_sessions"`
	} `json:"cooldown"`
	MaxRisk struct {
		Rule           int             `json:"rule"`
		PerPositionCap decimal.Decimal `json:"per_position_cap"`
		AggregateCap   decimal.Decimal `json:"aggregate_cap"`
	} `json:"max_risk"`
}

// Band is an inclusive delta range.
type Band struct {
	Rule string          `json:"rule"`
	Min  decimal.Decimal `json:"min"`
	Max  decimal.Decimal `json:"max"`
}

func (b Band) valid() bool { return b.Min.IsPositive() && b.Max.GreaterThan(b.Min) }

// RequiredClearance is the minimum strike clearance over breakeven (Rule 11
// graded clearance) for a block whose spot is `depth` underwater
// ((basis - spot) / basis). Linear between the near-basis and deep thresholds.
func (r *Rules) RequiredClearance(depth decimal.Decimal) decimal.Decimal {
	g := r.GradedClearance
	switch {
	case depth.LessThanOrEqual(g.NearBasisDepth):
		return g.NearBasisMin
	case depth.GreaterThanOrEqual(g.DeepDepth):
		return g.DeepMin
	}
	frac := g.DeepDepth.Sub(depth).Div(g.DeepDepth.Sub(g.NearBasisDepth))
	return g.DeepMin.Add(g.NearBasisMin.Sub(g.DeepMin).Mul(frac))
}

// Load reads and validates rules.json. Missing or zero values are errors, so
// a typo in the file can never silently disable a check.
func Load(path string) (*Rules, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(path, b)
}

// Parse decodes and validates rules.json bytes; path labels errors.
func Parse(path string, b []byte) (*Rules, error) {
	var r Rules
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := validateCaps(r.ConcentrationCaps); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	switch {
	case !r.CoveredCallCloser.FractionOfFill.IsPositive():
		return nil, fmt.Errorf("%s: covered_call_closer.fraction_of_fill must be > 0", path)
	case r.CoveredCallCloser.TimeInForce == "":
		return nil, fmt.Errorf("%s: covered_call_closer.time_in_force is required", path)
	case r.CoveredCallCloser.Rounding != "up_to_tick" && r.CoveredCallCloser.Rounding != "nearest_tick_half_up":
		return nil, fmt.Errorf("%s: unsupported closer rounding %q", path, r.CoveredCallCloser.Rounding)
	case !r.OptionTicks.Cutoff.IsPositive() || !r.OptionTicks.BelowCutoff.IsPositive() || !r.OptionTicks.AtOrAboveCutoff.IsPositive():
		return nil, fmt.Errorf("%s: option_ticks values must be > 0", path)
	case r.RollTrigger.ITMAndMaxDTE <= 0:
		return nil, fmt.Errorf("%s: roll_trigger.itm_and_max_dte must be > 0", path)
	case r.LotSize.SharesPerContract <= 0:
		return nil, fmt.Errorf("%s: lot_size.shares_per_contract must be > 0", path)
	case !r.DeltaBands.CoveredCall.valid() || !r.DeltaBands.AcquisitionPut.valid() || !r.DeltaBands.IncomePut.valid():
		return nil, fmt.Errorf("%s: every delta band needs 0 < min < max", path)
	case !r.IncomePutCloser.FractionOfFill.IsPositive() || r.IncomePutCloser.TimeInForce == "":
		return nil, fmt.Errorf("%s: income_put_closer needs a fraction and time_in_force", path)
	case !r.GradedClearance.NearBasisMin.IsPositive() || !r.GradedClearance.DeepDepth.GreaterThan(r.GradedClearance.NearBasisDepth) || r.GradedClearance.DeepMin.IsNegative():
		return nil, fmt.Errorf("%s: graded_clearance thresholds are inconsistent", path)
	case r.MinOpenInterest.Value <= 0:
		return nil, fmt.Errorf("%s: min_open_interest.value must be > 0", path)
	case r.QuoteWindow.Timezone == "" || r.QuoteWindow.Open == "" || r.QuoteWindow.Close == "":
		return nil, fmt.Errorf("%s: quote_window needs timezone, open and close", path)
	case r.Cooldown.Losses <= 0 || r.Cooldown.WithinSessions <= 0 || r.Cooldown.FreezeSessions <= 0 || !r.Cooldown.SingleLossShareOfPool.IsPositive():
		return nil, fmt.Errorf("%s: cooldown values must be > 0", path)
	case !r.MaxRisk.PerPositionCap.IsPositive() || !r.MaxRisk.AggregateCap.IsPositive():
		return nil, fmt.Errorf("%s: max_risk caps must be > 0", path)
	}
	if _, err := time.LoadLocation(r.QuoteWindow.Timezone); err != nil {
		return nil, fmt.Errorf("%s: quote_window.timezone: %w", path, err)
	}
	return &r, nil
}

// Tick returns the option price increment that applies at price p.
func (r *Rules) Tick(p decimal.Decimal) decimal.Decimal {
	if p.LessThan(r.OptionTicks.Cutoff) {
		return r.OptionTicks.BelowCutoff
	}
	return r.OptionTicks.AtOrAboveCutoff
}

// RoundToTick rounds a closer price to the option tick using the configured
// rounding: "up_to_tick" (never below p, so the profit target is a ceiling)
// or "nearest_tick_half_up". The tick is chosen from the unrounded price.
func (r *Rules) RoundToTick(p decimal.Decimal) decimal.Decimal {
	t := r.Tick(p)
	q := p.Div(t)
	if r.CoveredCallCloser.Rounding == "up_to_tick" {
		return q.Ceil().Mul(t)
	}
	return q.Round(0).Mul(t)
}

// ConcentrationCap limits the share of a pool's (and the book's) invested equity
// that one named group of symbols may hold. Over the cap, the check-in raises an
// alert and the ticket refuses new puts in those symbols in that pool.
type ConcentrationCap struct {
	Name     string          `json:"name"`
	Rule     int             `json:"rule"` // the the rulebook rule number; 0 when there is none
	Symbols  []string        `json:"symbols"`
	MaxShare decimal.Decimal `json:"max_share_of_invested_equity"`
}

// Has reports whether sym is in the cap's group.
func (c ConcentrationCap) Has(sym string) bool { return slices.Contains(c.Symbols, sym) }

// CapsFor lists the caps whose group includes sym.
func (r *Rules) CapsFor(sym string) []ConcentrationCap {
	var out []ConcentrationCap
	for _, c := range r.ConcentrationCaps {
		if c.Has(sym) {
			out = append(out, c)
		}
	}
	return out
}

// validateCaps requires every cap to be complete and the names to be unique: the
// name is part of a finding's subject, so two caps sharing one would collide.
func validateCaps(caps []ConcentrationCap) error {
	seen := map[string]bool{}
	for i, c := range caps {
		switch {
		case c.Name == "":
			return fmt.Errorf("concentration_caps[%d] needs a name", i)
		case seen[c.Name]:
			return fmt.Errorf("concentration_caps: name %q appears twice", c.Name)
		case len(c.Symbols) == 0:
			return fmt.Errorf("concentration_caps[%d] (%s) needs symbols", i, c.Name)
		case !c.MaxShare.IsPositive() || c.MaxShare.GreaterThan(decimal.NewFromInt(1)):
			return fmt.Errorf("concentration_caps[%d] (%s): max_share_of_invested_equity must be > 0 and ≤ 1", i, c.Name)
		}
		seen[c.Name] = true
	}
	return nil
}
