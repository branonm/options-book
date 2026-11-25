package wheel

import (
	"testing"

	"github.com/branonm/options-book/internal/ledger"
	"github.com/branonm/options-book/internal/snapshot"
)

// Figures are invented and worked by hand from the synthetic rules (see
// ticket_test.go). The block: 300 AAA, basis $60, $900 of premium, so the
// breakeven is $57.00; spot $54 is 10% underwater, where the graded clearance
// minimum is 5% − (10−3)/(12−3) × 4% ≈ 1.89%.
func recOptions() []*snapshot.Option {
	os := []*snapshot.Option{
		// limit .95, BE after 56.05: strike under it.
		option("AAA", "call", "56.0000", "2026-12-18", "0.90", "1.00", "0.35", 500, sessionQuote),
		// limit .74 (mid .745 down to the tick), BE after 56.26, clearance 3.09%, in band.
		option("AAA", "call", "58.0000", "2026-12-18", "0.70", "0.79", "0.25", 5000, sessionQuote),
		// limit .42, Δ .15 under the band; dropped, since the $58 is in band (Rule 9).
		option("AAA", "call", "60.0000", "2026-12-18", "0.40", "0.44", "0.15", 1000, sessionQuote),
		// open interest 10 < 50.
		option("AAA", "call", "62.0000", "2026-12-18", "0.20", "0.24", "0.10", 10, sessionQuote),
		// limit 1.15, BE after 55.85, clearance 5.64%, in band.
		option("AAA", "call", "59.0000", "2027-01-15", "1.10", "1.20", "0.30", 800, sessionQuote),
		// The only strike at 2/19, Δ .15 under the band: kept, as nothing in
		// band passes there. Limit .85, BE after 56.15, clearance 8.64%.
		option("AAA", "call", "61.0000", "2027-02-19", "0.80", "0.90", "0.15", 300, sessionQuote),
		// no bid.
		option("AAA", "call", "64.0000", "2027-01-15", "0.00", "0.05", "0.05", 800, sessionQuote),
		// ignored: a put, and another underlying.
		option("AAA", "put", "50.0000", "2026-12-18", "0.90", "1.00", "0.30", 800, sessionQuote),
		option("BBB", "call", "58.0000", "2026-12-18", "0.90", "1.00", "0.30", 800, sessionQuote),
	}
	for i, o := range os {
		id := string(rune('a' + i))
		o.Instrument.ID, o.Quote.InstrumentID = id, id
	}
	return os
}

func TestSellLimit(t *testing.T) {
	r := loadRules(t)
	o := recOptions()
	want(t, "mid .745 floors to the penny", SellLimit(r, o[1]), "0.74")
	want(t, "mid .42", SellLimit(r, o[2]), "0.42")
	want(t, "no bid", SellLimit(r, o[6]), "0")
	hi := option("AAA", "call", "50.0000", "2026-12-18", "4.10", "4.30", "0.6", 100, sessionQuote)
	want(t, "mid 4.20 at or above $3 floors to the nickel", SellLimit(r, hi), "4.20")
	hi = option("AAA", "call", "50.0000", "2026-12-18", "4.10", "4.25", "0.6", 100, sessionQuote)
	want(t, "mid 4.175 floors to 4.15, at or above the bid", SellLimit(r, hi), "4.15")
	hi = option("AAA", "call", "50.0000", "2026-12-18", "4.12", "4.14", "0.6", 100, sessionQuote)
	want(t, "the floor never goes under the bid", SellLimit(r, hi), "4.12")
}

func TestRecommend(t *testing.T) {
	r := loadRules(t)
	bk := ticketBook("AAA", 300, "60", "900", "54", "100000", "5000")
	recs, err := bk.Recommend(r, RecommendRequest{Symbol: "aaa", Options: recOptions()})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d recommendations, want 1", len(recs))
	}
	rc := recs[0]
	if rc.FreeLots != 3 || rc.Considered != 7 || rc.Rejected != 4 || len(rc.Candidates) != 3 {
		t.Fatalf("free %d considered %d rejected %d candidates %d; want 3, 7, 4, 3 (reasons %v)",
			rc.FreeLots, rc.Considered, rc.Rejected, len(rc.Candidates), rc.Reasons)
	}
	for _, k := range []string{"strike below the breakeven (Rule 11)", "open interest below the minimum", "no bid", reasonUnderBand} {
		if rc.Reasons[k] != 1 {
			t.Errorf("reason %q ×%d, want ×1 (all: %v)", k, rc.Reasons[k], rc.Reasons)
		}
	}
	type row struct {
		exp, strike   string
		best, inBand  bool
		limit, credit string
	}
	exp := []row{
		{"2026-12-18", "58", true, true, "0.74", "222"},
		{"2027-01-15", "59", true, true, "1.15", "345"},
		{"2027-02-19", "61", true, false, "0.85", "255"},
	}
	for i, e := range exp {
		c := rc.Candidates[i]
		if c.Expiry != e.exp || !c.Strike.Equal(d(e.strike)) || c.Best != e.best || c.InBand != e.inBand {
			t.Errorf("candidate %d = %s $%s best=%v inBand=%v; want %s $%s best=%v inBand=%v",
				i, c.Expiry, c.Strike, c.Best, c.InBand, e.exp, e.strike, e.best, e.inBand)
		}
		want(t, "limit", c.Limit, e.limit)
		want(t, "credit", c.Credit, e.credit)
		if c.Qty != 3 {
			t.Errorf("candidate %d qty %d, want every free lot (3)", i, c.Qty)
		}
	}
	want(t, "BE after the $58 call", *rc.Candidates[0].BreakevenAfter, "56.26")
	want(t, "BE after the $61 call", *rc.Candidates[2].BreakevenAfter, "56.15")
	// Every rung is empty: the nearest expiry wins.
	if rc.Pick == nil || rc.Pick.Expiry != "2026-12-18" || !rc.Pick.Strike.Equal(d("58")) {
		t.Fatalf("pick = %+v, want 2026-12-18 $58", rc.Pick)
	}
}

// One contract already on 12/18 makes 1/15 the nearest empty rung (Rule 1),
// and leaves two free lots.
func TestRecommendLadder(t *testing.T) {
	r := loadRules(t)
	bk := ticketBook("AAA", 300, "60", "900", "54", "100000", "5000")
	b := bk.Pools[0].Accounts[0].Blocks[0]
	b.Calls = append(b.Calls, &ShortCall{Leg: ledger.Leg{
		Key:     ledger.ContractKey{Symbol: "AAA", Type: "call", Strike: "61", Expiry: "2026-12-18"},
		OpenQty: 1, FillPerShare: d("0.50")}, Strike: d("61"), block: b})
	recs, err := bk.Recommend(r, RecommendRequest{Symbol: "AAA", Account: "roth", Options: recOptions()})
	if err != nil {
		t.Fatal(err)
	}
	rc := recs[0]
	if rc.FreeLots != 2 {
		t.Errorf("free lots %d, want 2", rc.FreeLots)
	}
	if rc.Pick == nil || rc.Pick.Expiry != "2027-01-15" || rc.Pick.Rung != 0 {
		t.Fatalf("pick = %+v, want 2027-01-15 on an empty rung", rc.Pick)
	}
	if rc.Candidates[0].Rung != 1 {
		t.Errorf("12/18 rung = %d, want 1", rc.Candidates[0].Rung)
	}
}

func TestRecommendNoLots(t *testing.T) {
	r := loadRules(t)
	bk := ticketBook("AAA", 300, "60", "900", "54", "100000", "5000")
	if _, err := bk.Recommend(r, RecommendRequest{Symbol: "ZZZ", Options: recOptions()}); err == nil {
		t.Error("no shares of ZZZ: want an error")
	}
	if _, err := bk.Recommend(r, RecommendRequest{Symbol: "AAA", Account: "nope", Options: recOptions()}); err == nil {
		t.Error("unknown account: want an error")
	}
}

// B11: the nearest expiry's only strike is under the band (the Rule 11 floor
// forces it there), while a later expiry on an equally empty rung has a strike
// in band. The pick is the in-band one, not the nearer, weaker one. When
// nothing is in band the nearer expiry still wins.
func TestRecommendPicksInBandOverNearerUnderBand(t *testing.T) {
	r := loadRules(t)
	bk := ticketBook("AAA", 300, "60", "900", "54", "100000", "5000")
	os := []*snapshot.Option{
		// 12/18: only a Δ .15 strike passes, so it stays, under the band.
		option("AAA", "call", "60.0000", "2026-12-18", "0.40", "0.44", "0.15", 1000, sessionQuote),
		// 1/15: in band, on an equally empty rung.
		option("AAA", "call", "59.0000", "2027-01-15", "1.10", "1.20", "0.30", 800, sessionQuote),
	}
	for i, o := range os {
		id := string(rune('a' + i))
		o.Instrument.ID, o.Quote.InstrumentID = id, id
	}
	recs, err := bk.Recommend(r, RecommendRequest{Symbol: "AAA", Options: os})
	if err != nil {
		t.Fatal(err)
	}
	rc := recs[0]
	if len(rc.Candidates) != 2 || rc.Candidates[0].InBand || !rc.Candidates[1].InBand {
		t.Fatalf("candidates = %+v, want 12/18 under the band and 1/15 in band", rc.Candidates)
	}
	if rc.Pick == nil || rc.Pick.Expiry != "2027-01-15" || !rc.Pick.Strike.Equal(d("59")) {
		t.Fatalf("pick = %+v, want 2027-01-15 $59 (in band)", rc.Pick)
	}

	// Nothing in band anywhere: the nearer expiry on the emptiest rung wins.
	os[1] = option("AAA", "call", "59.0000", "2027-01-15", "1.10", "1.20", "0.15", 800, sessionQuote)
	os[1].Instrument.ID, os[1].Quote.InstrumentID = "b", "b"
	recs, err = bk.Recommend(r, RecommendRequest{Symbol: "AAA", Options: os})
	if err != nil {
		t.Fatal(err)
	}
	if p := recs[0].Pick; p == nil || p.Expiry != "2026-12-18" {
		t.Fatalf("pick = %+v, want 2026-12-18 when nothing is in band", p)
	}
}
