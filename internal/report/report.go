// Package report produces the output of every figure-producing command:
// ledger, checkin, ticket, book, diff and reconcile. The `wheel` CLI and the
// ob-mcp server both call these functions with the same inputs, so a tool
// returns exactly the bytes the CLI prints (the storage design phase 3).
//
// Each function reads its snapshot through a snapshot.At (any Source) and
// writes to w. It returns an exit code where the CLI has one: 1 means "the
// book is wrong" (defects, a rejected ticket, a reconcile mismatch), not an
// error.
package report

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/branonm/options-book/internal/layout"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/shopspring/decimal"

	"github.com/branonm/options-book/internal/broker"
	"github.com/branonm/options-book/internal/config"
	"github.com/branonm/options-book/internal/diff"
	"github.com/branonm/options-book/internal/format"
	"github.com/branonm/options-book/internal/history"
	"github.com/branonm/options-book/internal/ledger"
	"github.com/branonm/options-book/internal/reconcile"
	"github.com/branonm/options-book/internal/render"
	"github.com/branonm/options-book/internal/rules"
	"github.com/branonm/options-book/internal/snapshot"
	"github.com/branonm/options-book/internal/strategy"
	"github.com/branonm/options-book/internal/strategy/wheel"
)

// ResolveAsOf is the only place an as-of date is decided. An explicit asof
// (YYYY-MM-DD) wins; otherwise it is the snapshot id's session date. There is
// no fallback to the clock: a name that isn't a snapshot id is an error (B1, B3).
func ResolveAsOf(snap, asof string) (time.Time, error) {
	if asof != "" {
		t, err := time.Parse("2006-01-02", asof)
		if err != nil {
			return time.Time{}, fmt.Errorf("--asof: %w", err)
		}
		return t, nil
	}
	id, err := snapshot.ParseID(snap)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w (or pass --asof YYYY-MM-DD)", err)
	}
	return id.Date, nil
}

// Assemble loads a snapshot's accounts and builds the book as of asOf.
func Assemble(cfg *config.Book, at snapshot.At, asOf time.Time) (*snapshot.Snapshot, *wheel.Book, error) {
	var names []string
	for _, a := range cfg.Accounts {
		names = append(names, a.Name)
	}
	at.Broker = cfg.Source()
	snap, err := snapshot.LoadAt(at, names)
	if err != nil {
		return nil, nil, err
	}
	switch cfg.Strategy {
	case strategy.Wheel:
		bk, err := wheel.Assemble(cfg, snap, asOf)
		return snap, bk, err
	}
	return nil, nil, fmt.Errorf("book %s: no implementation for strategy %q", cfg.Name, cfg.Strategy)
}

func day(t time.Time) string { return t.Format("2006-01-02") }

func emit(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func usd(d decimal.Decimal) string { return format.USD(d) }

func pct(m, t decimal.Decimal) string {
	if t.IsZero() {
		return "n/a"
	}
	return m.Div(t).Shift(2).StringFixed(1) + "%"
}

func pctOf(d decimal.Decimal) string { return d.Shift(2).StringFixed(1) + "%" }

// Ledger prints one account's ledger, or every account's when only is "".
func Ledger(w io.Writer, cfg *config.Book, at snapshot.At, asOf time.Time, only string, asJSON bool) error {
	_, bk, err := Assemble(cfg, at, asOf)
	if err != nil {
		return err
	}
	var out []*ledger.AccountLedger
	for _, a := range bk.Accounts() {
		if only == "" || a.Name == only {
			out = append(out, a.Ledger)
			if !asJSON {
				printLedger(w, a.Label, day(asOf), a.Ledger)
				fmt.Fprintln(w)
			}
		}
	}
	if len(out) == 0 {
		return fmt.Errorf("no account %q in the snapshot", only)
	}
	if asJSON {
		return emit(w, out)
	}
	return nil
}

// Checkin prints defects, alerts, closers, premium, concentration shares and the
// ladder. Exit code 1 when there is any defect.
func Checkin(w io.Writer, cfg *config.Book, r *rules.Rules, at snapshot.At, asOf time.Time, asJSON bool) (int, error) {
	_, bk, err := Assemble(cfg, at, asOf)
	if err != nil {
		return 0, err
	}
	rep := bk.Check(r)
	if asJSON {
		if err := emit(w, struct {
			AsOf   string       `json:"as_of"`
			Report wheel.Report `json:"report"`
			Ladder []wheel.Rung `json:"ladder"`
		}{day(asOf), rep, bk.Ladder()}); err != nil {
			return 0, err
		}
	} else {
		printCheckin(w, day(asOf), r, bk, rep)
	}
	if len(rep.Defects) > 0 {
		return 1, nil
	}
	return 0, nil
}

func printCheckin(out io.Writer, asof string, r *rules.Rules, bk *wheel.Book, rep wheel.Report) {
	fmt.Fprintf(out, "# Check-in as of %s\n\n", asof)
	fmt.Fprintf(out, "%d defects · %d alerts · %d checks passed\n", len(rep.Defects), len(rep.Alerts), rep.Passed)

	section := func(title string, fs []wheel.Finding) {
		if len(fs) == 0 {
			return
		}
		fmt.Fprintf(out, "\n## %s\n", title)
		w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ACCOUNT\tCHECK\tSUBJECT\tEXPECTED\tACTUAL\tRULE")
		for _, f := range fs {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", f.Account, f.Check, f.Subject, f.Expected, f.Actual, f.Rule)
		}
		w.Flush()
	}
	section("Defects (fix the order)", rep.Defects)
	section("Alerts (judgment needed)", rep.Alerts)

	fmt.Fprintln(out, "\n## Closers (Rule 10)")
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ACCOUNT\tLEG\tQTY\tFILL\tEXPECTED\tLIVE\tTIF\tDTE")
	for _, a := range bk.Accounts() {
		for _, b := range a.Blocks {
			for _, c := range b.Calls {
				want := c.ExpectedCloser(r)
				live, tif := "none", ""
				if len(c.Closers) > 0 {
					live, tif = "$"+c.Closers[0].Price.StringFixed(2), c.Closers[0].Tif
				}
				fmt.Fprintf(w, "%s\t%s\t%d\t$%s\t$%s\t%s\t%s\t%d\n", a.Name, c.Leg.Key, c.Leg.OpenQty, c.Leg.FillPerShare.StringFixed(2), want.Price.StringFixed(2), live, tif, c.DTE(bk))
			}
		}
	}
	w.Flush()

	op := bk.OpenPremium(r)
	fmt.Fprintln(out, "\n## Premium")
	fmt.Fprintf(out, "  Realized (all accounts):         %s\n", usd(op.RealizedSoFar))
	fmt.Fprintf(out, "  Open short-call credit (gross):  %s\n", usd(op.Gross))
	fmt.Fprintf(out, "  Open net at live closers:        %s", usd(op.NetAtLive))
	if op.MissingLive > 0 {
		fmt.Fprintf(out, "  (%d calls have no closer; not included)", op.MissingLive)
	}
	fmt.Fprintf(out, "\n  Open net at Rule 10 closers:     %s\n", usd(op.NetAtRule10))
	fmt.Fprintf(out, "  Expected total (realized + open net at live closers): %s\n", usd(op.RealizedSoFar.Add(op.NetAtLive)))

	for _, c := range r.ConcentrationCaps {
		fmt.Fprintf(out, "\n## Concentration: %s\n", c.Name)
		for _, p := range bk.Pools {
			g, t := p.CapShare(c)
			fmt.Fprintf(out, "  %-12s %s  (%s of %s)\n", p.Name, pct(g, t), usd(g), usd(t))
		}
		g, t := bk.CapShare(c)
		fmt.Fprintf(out, "  %-12s %s  (%s of %s)\n", "book", pct(g, t), usd(g), usd(t))
	}
	fmt.Fprintln(out, "  Prices are the snapshot's quotes; intraday snapshots are provisional.")

	fmt.Fprintln(out, "\n## Ladder (open short-call contracts)")
	for _, rg := range bk.Ladder() {
		fmt.Fprintf(out, "  %s  %d\n", rg.Expiry, rg.Contracts)
	}
}

func printLedger(out io.Writer, label, asof string, l *ledger.AccountLedger) {
	fmt.Fprintf(out, "# %s ledger as of %s\n\n", label, asof)
	fmt.Fprintf(out, "Realized premium: %s over %d legs (%d profitable)\n", usd(l.Realized), l.RealizedLegs, l.ProfitableLegs)
	fmt.Fprintf(out, "Open-leg credit:  %s\n", usd(l.OpenCredit))
	fmt.Fprintf(out, "Acquisition-put credit carried in share basis (excluded): %s\n", usd(l.BasisCredit))
	fmt.Fprintf(out, "Pre-campaign legs excluded: %d\n\n", l.PreCampaign)

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "BLOCK\tSHARES\tBASIS\tPREMIUM\tBREAKEVEN\tLEGS\tSINCE")
	for _, b := range l.Blocks {
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\t%d\t%s\n", b.Symbol, b.Shares, usd(b.Basis), usd(b.Premium), usd(b.Breakeven), b.Legs, b.Since)
	}
	w.Flush()

	fmt.Fprintln(out, "\nOpen legs:")
	w = tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "LEG\tQTY\tFILL/SH\tCREDIT\tOPENED")
	for _, lg := range l.Legs {
		if lg.End != ledger.EndOpen || lg.PreCampaign {
			continue
		}
		fmt.Fprintf(w, "%s\t%d\t$%s\t%s\t%s\n", lg.Key, lg.OpenQty, lg.FillPerShare.StringFixed(4), usd(lg.Net), lg.OpenedAt)
	}
	w.Flush()

	var losses []ledger.Leg
	for _, lg := range l.Legs {
		if !lg.PreCampaign && lg.End != ledger.EndOpen && lg.Kind != ledger.KindAcquisitionPut && !lg.Net.IsPositive() {
			losses = append(losses, lg)
		}
	}
	sort.Slice(losses, func(i, j int) bool { return losses[i].OpenedAt < losses[j].OpenedAt })
	if len(losses) > 0 {
		fmt.Fprintln(out, "\nLegs that did not make money:")
		for _, lg := range losses {
			fmt.Fprintf(out, "  %s  net %s (closed %s)\n", lg.Key, usd(lg.Net), lg.ClosedAt)
		}
	}
	if len(l.Warnings) > 0 {
		fmt.Fprintln(out, "\nWarnings:")
		for _, s := range l.Warnings {
			fmt.Fprintln(out, "  -", s)
		}
	}
}

// TicketRequest is a proposed trade, as the CLI flags and the tool take it.
type TicketRequest struct {
	Account, Kind, Option, Limit string
	Qty                          int64
}

// Ticket builds a full ticket or a rejection with reasons. Exit code 1 when
// the ticket is rejected.
func Ticket(w io.Writer, cfg *config.Book, r *rules.Rules, at snapshot.At, asOf time.Time, req TicketRequest, asJSON bool) (int, error) {
	if req.Account == "" || req.Kind == "" || req.Option == "" || req.Limit == "" {
		return 0, fmt.Errorf("a ticket needs account, kind, option and limit")
	}
	if _, err := decimal.NewFromString(req.Limit); err != nil {
		return 0, fmt.Errorf("--limit: %w", err)
	}
	// Assemble first, so a bad snapshot is reported before a missing option, as before.
	if _, _, err := Assemble(cfg, at, asOf); err != nil {
		return 0, err
	}
	at.Broker = cfg.Source()
	opt, err := snapshot.LoadOptionAt(at, req.Option)
	if err != nil {
		return 0, err
	}
	return TicketOption(w, cfg, r, at, asOf, req, opt, time.Time{}, asJSON)
}

// TicketOption is Ticket for an option already resolved: from the snapshot
// (Ticket), or fetched by the server itself, when fetched is when it fetched
// the quote and the quote window is checked against that (PLAN.md addendum A).
func TicketOption(w io.Writer, cfg *config.Book, r *rules.Rules, at snapshot.At, asOf time.Time, req TicketRequest, opt *snapshot.Option, fetched time.Time, asJSON bool) (int, error) {
	lim, err := decimal.NewFromString(req.Limit)
	if err != nil {
		return 0, fmt.Errorf("--limit: %w", err)
	}
	_, bk, err := Assemble(cfg, at, asOf)
	if err != nil {
		return 0, err
	}
	t, err := bk.Ticket(r, wheel.Proposal{Account: req.Account, Kind: req.Kind, Option: opt, Limit: lim, Qty: req.Qty, FetchedAt: fetched})
	if err != nil {
		return 0, err
	}
	ex := BuildExecution(cfg, t)
	if asJSON {
		if err := emit(w, ticketJSON{t, ex}); err != nil {
			return 0, err
		}
	} else {
		printTicket(w, t)
		if ex != nil {
			printExecution(w, ex)
		}
	}
	if !t.Accepted {
		return 1, nil
	}
	return 0, nil
}

func printTicket(out io.Writer, t *wheel.Ticket) {
	verdict := "ACCEPTED"
	if !t.Accepted {
		verdict = "REJECTED"
	}
	fmt.Fprintf(out, "# Ticket: %s %s, %s — %s\n\n", t.Account, t.Kind, t.Contract, verdict)
	for _, s := range t.Rejections {
		fmt.Fprintln(out, "  ✗", s)
	}
	for _, s := range t.Warnings {
		fmt.Fprintln(out, "  !", s)
	}
	if len(t.Rejections)+len(t.Warnings) > 0 {
		fmt.Fprintln(out)
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	row := func(k, v string) { fmt.Fprintf(w, "%s\t%s\n", k, v) }
	row("Order", fmt.Sprintf("sell to open %d × %s @ $%s limit", t.Qty, t.Contract, t.Limit.StringFixed(2)))
	row("Credit", usd(t.Credit))
	row("Quote", fmt.Sprintf("bid $%s · ask $%s · mark $%s · Δ %s · OI %d · %s", t.Bid.StringFixed(2), t.Ask.StringFixed(2), t.Mark.StringFixed(2), t.Delta.StringFixed(3), t.OI, t.QuoteTime))
	row("Days to expiry", fmt.Sprint(t.DTE))
	if t.BreakevenAfter != nil {
		row("Basis", usd(*t.Basis))
		row("Breakeven (Rule 11)", fmt.Sprintf("%s → %s after this credit", usd(*t.BreakevenBefore), usd(*t.BreakevenAfter)))
		cl := fmt.Sprintf("%s/share (%s)", usd(*t.Clearance), pctOf(*t.ClearancePct))
		if t.RequiredClearance != nil {
			cl += fmt.Sprintf("; required %s at %s underwater (spot %s)", pctOf(*t.RequiredClearance), pctOf(*t.DepthUnderwaterPct), usd(*t.Spot))
		}
		row("Strike clearance", cl)
	}
	if ca := t.CallAway; ca != nil {
		row("If called", fmt.Sprintf("share gain %s + premium %s = %s", usd(ca.ShareGain), usd(ca.Premium), usd(ca.Total)))
		row("", fmt.Sprintf("%s annualized on %s capital (%d shares × basis) over %d calendar days, 365-day year", pctOf(ca.Annualized), usd(ca.Capital), ca.SharesCalled, ca.Days))
	}
	if t.EffectiveCost != nil {
		row("Effective cost", usd(*t.EffectiveCost)+"/share if assigned")
		bp := "unknown"
		if t.BuyingPower != nil {
			bp = usd(*t.BuyingPower)
		}
		row("Collateral", fmt.Sprintf("%s needed; buying power %s", usd(*t.CashRequired), bp))
	}
	row("Max loss (Rule 6)", fmt.Sprintf("%s = %s of pool equity %s (%s)", usd(t.MaxLoss), pctOf(t.MaxLossPctPool), usd(t.PoolEquity), t.PoolEquitySource))
	if t.Closer != nil {
		row("Closer", fmt.Sprintf("buy to close %d @ $%s, %s", t.Closer.Qty, t.Closer.Price.StringFixed(2), t.Closer.Tif))
	}
	row("", t.CloserRule)
	w.Flush()
}

// Book renders the generated wheel-book tables. label names the snapshot in
// the provenance line; it is "snapshots/<id>" from any store.
func Book(w io.Writer, cfg *config.Book, r *rules.Rules, at snapshot.At, asOf time.Time, label string) error {
	_, bk, err := Assemble(cfg, at, asOf)
	if err != nil {
		return err
	}
	var b strings.Builder
	render.WheelBook(&b, bk, r, bk.Check(r), label)
	_, err = io.WriteString(w, b.String())
	return err
}

// CheckOrder requires from to be strictly earlier than to, using the same id
// parser as every other command.
func CheckOrder(from, to string) error {
	a, err := snapshot.ParseID(from)
	if err != nil {
		return err
	}
	b, err := snapshot.ParseID(to)
	if err != nil {
		return err
	}
	before, err := a.Before(b)
	if err != nil {
		return err
	}
	if !before {
		return fmt.Errorf("--from %s is not earlier than --to %s", a.Name, b.Name)
	}
	return nil
}

// Diff reports what changed between two snapshots, each dated by its id.
func Diff(w io.Writer, cfg *config.Book, r *rules.Rules, from, to snapshot.At) error {
	if err := CheckOrder(path.Base(from.Dir), path.Base(to.Dir)); err != nil {
		return err
	}
	side := func(at snapshot.At) (diff.Side, error) {
		asOf, err := ResolveAsOf(path.Base(at.Dir), "")
		if err != nil {
			return diff.Side{}, err
		}
		snap, bk, err := Assemble(cfg, at, asOf)
		return diff.Side{Label: path.Base(at.Dir), Book: bk, Snap: snap}, err
	}
	a, err := side(from)
	if err != nil {
		return err
	}
	b, err := side(to)
	if err != nil {
		return err
	}
	diff.Compare(a, b, r).Markdown(w)
	return nil
}

// PnL is one account's saved get_realized_pnl response.
type PnL struct {
	Account string
	Resp    broker.RealizedPnL
}

// Reconcile compares the ledger's realized premium with Robinhood's own
// realized option P&L. Exit code 1 on any mismatch.
func Reconcile(out io.Writer, cfg *config.Book, at snapshot.At, asOf time.Time, pnls []PnL, asJSON bool) (int, error) {
	if len(pnls) == 0 {
		return 0, fmt.Errorf("reconcile needs at least one account's realized P&L")
	}
	_, bk, err := Assemble(cfg, at, asOf)
	if err != nil {
		return 0, err
	}
	var lines []reconcile.Line
	for _, p := range pnls {
		a, _ := bk.Account(p.Account)
		if a == nil {
			return 0, fmt.Errorf("no account %q", p.Account)
		}
		line, err := reconcile.Account(a.Ledger, p.Resp)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", p.Account, err)
		}
		lines = append(lines, line)
	}
	if asJSON {
		if err := emit(out, lines); err != nil {
			return 0, err
		}
	} else {
		w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ACCOUNT\tWINDOW\tLEDGER\t− ASSIGNED CALLS\t= EXPECTED\tROBINHOOD\tDIFF\t")
		for _, l := range lines {
			mark := "✓"
			if !l.OK() {
				mark = "✗ MISMATCH"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", l.Account, l.Window, usd(l.LedgerRealized), usd(l.AssignedCalls), usd(l.Expected), usd(l.Robinhood), usd(l.Difference), mark)
		}
		w.Flush()
		for _, l := range lines {
			for _, x := range l.Excluded {
				fmt.Fprintf(out, "  %s: assigned call, premium booked by Robinhood in the share sale: %s\n", l.Account, x)
			}
		}
	}
	for _, l := range lines {
		if !l.OK() {
			return 1, nil
		}
	}
	return 0, nil
}

// ListSnapshots prints every snapshot in a Source with its seal state.
func ListSnapshots(out io.Writer, fsys fs.FS, ids []string) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SNAPSHOT\tSEALED\tFILES")
	for _, id := range ids {
		at := snapshot.At{FS: fsys, Dir: layout.Snapshot(id)}
		files, err := snapshotFiles(at)
		if err != nil {
			return err
		}
		sealed := "no"
		if m, err := snapshot.ReadManifestAt(at); err == nil {
			sealed = m.CreatedAt
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		fmt.Fprintf(w, "%s\t%s\t%d\n", id, sealed, len(files))
	}
	return w.Flush()
}

// GetSnapshot prints a snapshot's files with their sizes and sha256, its seal
// state, and — for each path in raw (relative to the snapshot) — the stored
// bytes verbatim.
func GetSnapshot(out io.Writer, at snapshot.At, raw []string) error {
	files, err := snapshotFiles(at)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "# Snapshot %s\n\n", path.Base(at.Dir))
	switch m, err := snapshot.ReadManifestAt(at); {
	case err == nil:
		fmt.Fprintf(out, "Sealed %s; the manifest fingerprints %d raw inputs.\n\n", m.CreatedAt, len(m.Files))
	case errors.Is(err, fs.ErrNotExist):
		fmt.Fprint(out, "Not sealed.\n\n")
	default:
		return err
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "FILE\tBYTES\tSHA256")
	for _, f := range files {
		b, err := fs.ReadFile(at.FS, path.Join(at.Dir, f))
		if err != nil {
			return err
		}
		s := sha256.Sum256(b)
		fmt.Fprintf(w, "%s\t%d\t%s\n", f, len(b), hex.EncodeToString(s[:]))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	for _, p := range raw {
		clean := path.Clean(p)
		if clean == "." || strings.HasPrefix(clean, "../") || path.IsAbs(clean) {
			return fmt.Errorf("%s: name a file inside the snapshot, e.g. roth/portfolio.json", p)
		}
		b, err := fs.ReadFile(at.FS, path.Join(at.Dir, clean))
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "\n## %s\n\n%s", clean, b)
		if len(b) > 0 && b[len(b)-1] != '\n' {
			fmt.Fprintln(out)
		}
	}
	return nil
}

func snapshotFiles(at snapshot.At) ([]string, error) {
	var files []string
	err := fs.WalkDir(at.FS, at.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		files = append(files, strings.TrimPrefix(p, at.Dir+"/"))
		return nil
	})
	sort.Strings(files)
	return files, err
}

// SnapshotProblems prints what `ob snapshot` (and seal_snapshot) found
// missing or inconsistent, with the call that fixes each.
func SnapshotProblems(out io.Writer, label string, ps []snapshot.Problem) {
	fmt.Fprintf(out, "# Snapshot %s: %d problems\n\n", label, len(ps))
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "FILE\tPROBLEM\tFIX")
	for _, p := range ps {
		fmt.Fprintf(w, "%s\t%s\t%s\n", p.File, p.Problem, p.Fix)
	}
	w.Flush()
}

// AccountRefs labels the configured accounts the way the snapshot check names them.
func AccountRefs(cfg *config.Book) []snapshot.AccountRef {
	var refs []snapshot.AccountRef
	for _, a := range cfg.Accounts {
		refs = append(refs, snapshot.AccountRef{Name: a.Name, Label: fmt.Sprintf("%s ••%s", a.Label, a.Last4)})
	}
	return refs
}

// HistoryPlan prints which pulls the next `history add` needs for an account.
// then is the closing instruction, which names the CLI or the tool.
func HistoryPlan(out io.Writer, account string, l *history.Log, then string) {
	p := l.NextPlan()
	if len(l.Entries) == 0 {
		fmt.Fprintf(out, "History for %s is empty: pull the full history with %s and run `ob history add --full`.\n", account, l.FullPull())
		return
	}
	fmt.Fprintf(out, "Next pull for %s (log has %d lines, last observed %s):\n", account, len(l.Entries), l.LastObserved())
	for _, line := range l.PlanSteps(p) {
		fmt.Fprintln(out, line)
	}
	fmt.Fprintln(out, then)
}
