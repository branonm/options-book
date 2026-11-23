package wheel

// Recommend: given an underlying and a set of call contracts with their
// quotes, list every strike and expiration a covered call could be written at,
// per account that holds free lots of it. Every candidate is run through
// Ticket, so the rules are checked in exactly one place; Recommend only picks
// the limit, filters and ranks.

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/branonm/options-book/internal/rules"
	"github.com/branonm/options-book/internal/snapshot"
)

// RecommendRequest names what to recommend for.
type RecommendRequest struct {
	Symbol  string
	Account string // "" = every account in the book with free lots of Symbol
	// Options are the candidate contracts with their quotes. Puts and other
	// underlyings are ignored.
	Options []*snapshot.Option
	// FetchedAt is when the quotes were fetched (see Proposal.FetchedAt).
	FetchedAt time.Time
}

// Candidate is one accepted covered call.
type Candidate struct {
	*Ticket
	// Rung is how many short-call contracts the book already has at this
	// expiration (Rule 1: write to the emptiest rung).
	Rung int64 `json:"rung_contracts"`
	// InBand is false when the delta is under the band, which is acceptable
	// only because the Rule 11 floor forces the strike up (Rule 9).
	InBand bool `json:"in_band"`
	// Best marks the pick for its expiration: the highest credit in band, or
	// the highest credit when nothing at that expiration is in band.
	Best bool `json:"best_for_expiry"`
}

// Recommendation is the list for one account.
type Recommendation struct {
	Account    string           `json:"account"`
	Symbol     string           `json:"symbol"`
	FreeLots   int64            `json:"free_lots"`
	Spot       *decimal.Decimal `json:"spot,omitempty"`
	Basis      decimal.Decimal  `json:"basis"`
	Breakeven  decimal.Decimal  `json:"breakeven"`
	Candidates []Candidate      `json:"candidates"` // by expiry, then strike
	// Pick is the suggested write: the best candidate on the emptiest rung,
	// the nearer expiration on a tie. nil when nothing passed.
	Pick *Candidate `json:"pick,omitempty"`
	// Considered and Rejected count the contracts tried; Reasons tallies the
	// first rejection reason of each, with its numbers stripped.
	Considered int            `json:"considered"`
	Rejected   int            `json:"rejected"`
	Reasons    map[string]int `json:"rejection_reasons"`
}

// SellLimit is the limit a recommendation quotes for a sell: the midpoint,
// rounded down to the contract's tick, never below the bid. Zero when the
// quote has no bid.
func SellLimit(r *rules.Rules, o *snapshot.Option) decimal.Decimal {
	bid, err1 := decimal.NewFromString(o.Quote.Bid.Text)
	ask, err2 := decimal.NewFromString(o.Quote.Ask.Text)
	if err1 != nil || err2 != nil || !bid.IsPositive() {
		return decimal.Zero
	}
	mid := bid
	if ask.GreaterThan(bid) {
		mid = bid.Add(ask).Div(decimal.NewFromInt(2))
	}
	in := o.Instrument
	tick := instrumentTick(in.Ticks.Cutoff.Text, in.Ticks.Below.Text, in.Ticks.Above.Text, mid)
	if !tick.IsPositive() {
		tick = r.Tick(mid)
	}
	lim := mid.Div(tick).Floor().Mul(tick)
	if lim.LessThan(bid) {
		lim = bid
	}
	return lim
}

// Recommend lists the covered calls that pass every ticket check.
func (bk *Book) Recommend(r *rules.Rules, req RecommendRequest) ([]Recommendation, error) {
	sym := strings.ToUpper(strings.TrimSpace(req.Symbol))
	if sym == "" {
		return nil, fmt.Errorf("recommend needs a symbol")
	}
	rungs := map[string]int64{}
	for _, g := range bk.Ladder() {
		rungs[g.Expiry] = g.Contracts
	}

	var accts []*Account
	for _, a := range bk.Accounts() {
		if req.Account == "" || a.Name == req.Account {
			accts = append(accts, a)
		}
	}
	if req.Account != "" && len(accts) == 0 {
		return nil, fmt.Errorf("no account %q in the book", req.Account)
	}

	var out []Recommendation
	for _, a := range accts {
		var blk *Block
		for _, b := range a.Blocks {
			if b.Symbol == sym {
				if blk != nil {
					return nil, fmt.Errorf("%s has more than one %s block; recommend can't tell which to write against", a.Name, sym)
				}
				blk = b
			}
		}
		if blk == nil {
			continue
		}
		var written int64
		for _, c := range blk.Calls {
			written += c.Leg.OpenQty
		}
		free := blk.Shares/r.LotSize.SharesPerContract - written
		if free <= 0 {
			continue
		}
		rec := Recommendation{Account: a.Name, Symbol: sym, FreeLots: free, Basis: blk.Basis, Breakeven: blk.Breakeven,
			Candidates: []Candidate{}, Reasons: map[string]int{}}
		if blk.HasPrice {
			rec.Spot = dp(blk.Price)
		}
		for _, o := range req.Options {
			if o.Instrument.Symbol != sym || o.Instrument.Right != "call" {
				continue
			}
			rec.Considered++
			lim := SellLimit(r, o)
			if !lim.IsPositive() {
				rec.Rejected++
				rec.Reasons["no bid"]++
				continue
			}
			t, err := bk.Ticket(r, Proposal{Account: a.Name, Kind: TradeCoveredCall, Option: o, Limit: lim, FetchedAt: req.FetchedAt})
			if err != nil {
				// One unreadable contract must not lose the rest of the list.
				rec.Rejected++
				rec.Reasons["unreadable contract"]++
				continue
			}
			if !t.Accepted {
				rec.Rejected++
				rec.Reasons[reasonKind(t)]++
				continue
			}
			rec.Candidates = append(rec.Candidates, Candidate{Ticket: t, Rung: rungs[t.Expiry],
				InBand: !t.Delta.LessThan(r.DeltaBands.CoveredCall.Min)})
		}
		dropForcedLow(&rec)
		rankCandidates(&rec)
		out = append(out, rec)
	}
	if len(out) == 0 {
		if req.Account != "" {
			return nil, fmt.Errorf("%s has no free lots of %s", req.Account, sym)
		}
		return nil, fmt.Errorf("no account in the book has free lots of %s", sym)
	}
	return out, nil
}

// reasonUnderBand is the tally key for a below-band strike dropped because an
// in-band strike at the same expiry also passed.
const reasonUnderBand = "delta under the band with an in-band strike at that expiry (Rule 9)"

// dropForcedLow applies Rule 9: a delta under the band is acceptable only
// when the Rule 11 floor forces the strike up, so at an expiry where some
// strike in band passes, the below-band strikes are dropped.
func dropForcedLow(rec *Recommendation) {
	inBand := map[string]bool{}
	for _, c := range rec.Candidates {
		if c.InBand {
			inBand[c.Expiry] = true
		}
	}
	kept := rec.Candidates[:0]
	for _, c := range rec.Candidates {
		if !c.InBand && inBand[c.Expiry] {
			rec.Rejected++
			rec.Reasons[reasonUnderBand]++
			continue
		}
		kept = append(kept, c)
	}
	rec.Candidates = kept
}

// rankCandidates sorts by expiry then strike, marks the best per expiry and
// picks the best on the emptiest rung.
func rankCandidates(rec *Recommendation) {
	cs := rec.Candidates
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].Expiry != cs[j].Expiry {
			return cs[i].Expiry < cs[j].Expiry
		}
		return cs[i].Strike.LessThan(cs[j].Strike)
	})
	best := map[string]int{}
	better := func(a, b Candidate) bool { // a beats b
		if a.InBand != b.InBand {
			return a.InBand
		}
		return a.Credit.GreaterThan(b.Credit)
	}
	for i, c := range cs {
		if j, ok := best[c.Expiry]; !ok || better(c, cs[j]) {
			best[c.Expiry] = i
		}
	}
	var pick *Candidate
	for _, i := range best {
		cs[i].Best = true
	}
	for i := range cs {
		c := &cs[i]
		if !c.Best {
			continue
		}
		if pick == nil || c.Rung < pick.Rung || (c.Rung == pick.Rung && c.Expiry < pick.Expiry) {
			pick = c
		}
	}
	if pick != nil {
		p := *pick
		rec.Pick = &p
	}
}

// reasonKind labels a ticket's first rejection so like reasons tally together.
// A classified rejection uses its kind; an unclassified one falls back to its
// text with the figures stripped.
func reasonKind(t *Ticket) string {
	if k := t.RejectionKind(0); k != "" {
		return k
	}
	s := t.Rejections[0]
	if i := strings.IndexAny(s, "0123456789$"); i > 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}
