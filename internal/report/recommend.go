package report

import (
	"fmt"
	"io"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/branonm/options-book/internal/config"
	"github.com/branonm/options-book/internal/rules"
	"github.com/branonm/options-book/internal/snapshot"
	"github.com/branonm/options-book/internal/strategy/wheel"
)

// Recommend lists the covered calls that pass every ticket check for a symbol,
// per account with free lots. Exit code 1 when no account has a candidate.
func Recommend(w io.Writer, cfg *config.Book, r *rules.Rules, at snapshot.At, asOf time.Time, req wheel.RecommendRequest, asJSON bool) (int, error) {
	_, bk, err := Assemble(cfg, at, asOf)
	if err != nil {
		return 0, err
	}
	recs, err := bk.Recommend(r, req)
	if err != nil {
		return 0, err
	}
	code := 1
	for _, rc := range recs {
		if rc.Pick != nil {
			code = 0
		}
	}
	if asJSON {
		return code, emit(w, recs)
	}
	for i, rc := range recs {
		if i > 0 {
			fmt.Fprintln(w)
		}
		printRecommendation(w, r, rc)
	}
	return code, nil
}

func printRecommendation(out io.Writer, r *rules.Rules, rc wheel.Recommendation) {
	spot := "no quote"
	if rc.Spot != nil {
		spot = usd(*rc.Spot)
	}
	fmt.Fprintf(out, "# %s %s — %d free lot(s) · spot %s · basis %s · breakeven %s\n\n",
		rc.Account, rc.Symbol, rc.FreeLots, spot, usd(rc.Basis), usd(rc.Breakeven))
	if len(rc.Candidates) == 0 {
		fmt.Fprintln(out, "No contract passed every check.")
	} else {
		b := r.DeltaBands.CoveredCall
		w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "\tEXPIRY\tDTE\tRUNG\tSTRIKE\tLIMIT\tBID/ASK\tΔ\tOI\tCREDIT\tBE AFTER\tCLEAR\tIF CALLED\t")
		for _, c := range rc.Candidates {
			mark := ""
			switch {
			case rc.Pick != nil && c.Contract == rc.Pick.Contract:
				mark = "★"
			case c.Best:
				mark = "•"
			}
			d := c.Delta.StringFixed(3)
			if !c.InBand {
				d += "↓"
			}
			called := "—"
			if c.CallAway != nil {
				called = fmt.Sprintf("%s (%s/yr)", usd(c.CallAway.Total), pctOf(c.CallAway.Annualized))
			}
			fmt.Fprintf(w, "%s\t%s\t%d\t%d\t$%s\t$%s\t%s/%s\t%s\t%d\t%s\t%s\t%s\t%s\t\n",
				mark, c.Expiry, c.DTE, c.Rung, c.Strike.String(), c.Limit.StringFixed(2),
				c.Bid.StringFixed(2), c.Ask.StringFixed(2), d, c.OI, usd(c.Credit),
				usd(*c.BreakevenAfter), pctOf(*c.ClearancePct), called)
		}
		w.Flush()
		fmt.Fprintf(out, "\n★ pick (best on the emptiest rung, Rule 1) · • best for its expiry · ↓ delta under the %s–%s band, allowed only because the Rule 11 floor forces the strike (Rule 9)\n", b.Min, b.Max)
		fmt.Fprintln(out, "LIMIT is the midpoint rounded down to the tick, never below the bid. RUNG is the book's open short calls at that expiry. Re-run ticket on the pick before entry.")
	}
	fmt.Fprintf(out, "Considered %d contracts; %d passed, %d rejected", rc.Considered, len(rc.Candidates), rc.Rejected)
	if len(rc.Reasons) > 0 {
		var ks []string
		for k := range rc.Reasons {
			ks = append(ks, k)
		}
		sort.Slice(ks, func(i, j int) bool {
			if rc.Reasons[ks[i]] != rc.Reasons[ks[j]] {
				return rc.Reasons[ks[i]] > rc.Reasons[ks[j]]
			}
			return ks[i] < ks[j]
		})
		fmt.Fprint(out, ":")
		for i, k := range ks {
			sep := ","
			if i == 0 {
				sep = ""
			}
			fmt.Fprintf(out, "%s %s ×%d", sep, k, rc.Reasons[k])
		}
	}
	fmt.Fprintln(out, ".")
}
