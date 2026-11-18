package risk

import (
	"testing"

	"github.com/shopspring/decimal"

	"github.com/branonm/options-book/internal/rules"
)

func dec(s string) decimal.Decimal {
	d, err := decimal.NewFromString(s)
	if err != nil {
		panic(err)
	}
	return d
}

// testRules matches config/rules.json's max_risk and a sample delta band.
func testRules() *rules.Rules {
	r := &rules.Rules{}
	r.MaxRisk.Rule = 6
	r.MaxRisk.PerPositionCap = dec("0.05")
	r.MaxRisk.AggregateCap = dec("0.15")
	return r
}

func TestCheckRejectsPoolEquityNotPositive(t *testing.T) {
	r := testRules()
	_, err := Check(r, Proposal{MaxLoss: dec("100")}, Portfolio{PoolEquity: dec("0")})
	if err == nil {
		t.Fatal("expected an error for non-positive pool equity")
	}
}

func TestCheckRejectsNegativeMaxLoss(t *testing.T) {
	r := testRules()
	_, err := Check(r, Proposal{MaxLoss: dec("-1")}, Portfolio{PoolEquity: dec("10000")})
	if err == nil {
		t.Fatal("expected an error for negative max loss")
	}
}

func TestCheckWithinCaps(t *testing.T) {
	r := testRules()
	res, err := Check(r, Proposal{Symbol: "XYZ", MaxLoss: dec("400")}, Portfolio{PoolEquity: dec("10000"), ExistingMaxLossUsed: dec("500")})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Accepted {
		t.Fatalf("expected accepted, got rejections %v", res.Rejections)
	}
	if !res.MaxLossPctPool.Equal(dec("0.04")) {
		t.Fatalf("got max loss pct %s", res.MaxLossPctPool)
	}
	if !res.AggregateIfAcceptedPct.Equal(dec("0.09")) {
		t.Fatalf("got aggregate pct %s", res.AggregateIfAcceptedPct)
	}
}

func TestCheckRejectsOverPerPositionCap(t *testing.T) {
	r := testRules()
	res, err := Check(r, Proposal{Symbol: "XYZ", MaxLoss: dec("600")}, Portfolio{PoolEquity: dec("10000")})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Accepted {
		t.Fatal("expected a rejection: 600/10000 = 6%, over the 5% per-position cap")
	}
	if len(res.Rejections) != 1 {
		t.Fatalf("got rejections %v", res.Rejections)
	}
}

func TestCheckRejectsOverAggregateCap(t *testing.T) {
	r := testRules()
	// Within the per-position cap (400/10000 = 4%) but existing exposure
	// pushes the aggregate over 15%.
	res, err := Check(r, Proposal{Symbol: "XYZ", MaxLoss: dec("400")}, Portfolio{PoolEquity: dec("10000"), ExistingMaxLossUsed: dec("1200")})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Accepted {
		t.Fatal("expected a rejection: (1200+400)/10000 = 16%, over the 15% aggregate cap")
	}
}

func TestCheckDeltaBand(t *testing.T) {
	r := testRules()
	band := &rules.Band{Rule: "test band", Min: dec("0.25"), Max: dec("0.35")}

	t.Run("within band", func(t *testing.T) {
		res, err := Check(r, Proposal{Symbol: "XYZ", MaxLoss: dec("100"), Delta: dec("0.30"), DeltaBand: band}, Portfolio{PoolEquity: dec("10000")})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !res.Accepted || len(res.Warnings) != 0 {
			t.Fatalf("got accepted=%v warnings=%v", res.Accepted, res.Warnings)
		}
	})

	t.Run("above band rejects", func(t *testing.T) {
		res, err := Check(r, Proposal{Symbol: "XYZ", MaxLoss: dec("100"), Delta: dec("0.40"), DeltaBand: band}, Portfolio{PoolEquity: dec("10000")})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Accepted {
			t.Fatal("expected a rejection for delta above the band")
		}
	})

	t.Run("below band warns, does not reject", func(t *testing.T) {
		res, err := Check(r, Proposal{Symbol: "XYZ", MaxLoss: dec("100"), Delta: dec("0.10"), DeltaBand: band}, Portfolio{PoolEquity: dec("10000")})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !res.Accepted {
			t.Fatalf("expected acceptance (a warning, not a rejection), got rejections %v", res.Rejections)
		}
		if len(res.Warnings) != 1 {
			t.Fatalf("got warnings %v", res.Warnings)
		}
	})

	t.Run("nil band skips the check", func(t *testing.T) {
		res, err := Check(r, Proposal{Symbol: "XYZ", MaxLoss: dec("100"), Delta: dec("99")}, Portfolio{PoolEquity: dec("10000")})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !res.Accepted {
			t.Fatalf("expected acceptance with no band set, got rejections %v", res.Rejections)
		}
	})
}
