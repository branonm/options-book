// Command ob computes wheel-book figures from a saved snapshot of Robinhood
// MCP responses. It never places orders and never contacts a broker.
//
//	ob ledger  --snapshot DIR [--account NAME] [--asof YYYY-MM-DD] [--json]
//	ob checkin --snapshot DIR [--asof YYYY-MM-DD] [--json]
//	ob ticket  --snapshot DIR --account NAME --kind KIND --option ID --limit PRICE [--qty N] [--json]
//	ob book    --snapshot DIR [--into FILE]
//	ob snapshot --snapshot DIR
//	ob diff    --from DIR --to DIR
//	ob history plan|add ...
//
// checkin exits 1 when the book has defects; ticket exits 1 when the ticket is rejected.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"github.com/branonm/options-book/internal/layout"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/branonm/options-book/internal/config"
	"github.com/branonm/options-book/internal/format"
	"github.com/branonm/options-book/internal/history"
	"github.com/branonm/options-book/internal/ledger"
	"github.com/branonm/options-book/internal/report"
	"github.com/branonm/options-book/internal/snapshot"
	"github.com/branonm/options-book/internal/strategy/wheel"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	code := 0
	switch os.Args[1] {
	case "ledger":
		err = runLedger(os.Args[2:])
	case "checkin":
		code, err = runCheckin(os.Args[2:])
	case "ticket":
		code, err = runTicket(os.Args[2:])
	case "book":
		err = runBook(os.Args[2:])
	case "snapshot":
		code, err = runSnapshot(os.Args[2:])
	case "diff":
		err = runDiff(os.Args[2:])
	case "history":
		err = runHistory(os.Args[2:])
	case "expirations":
		err = runExpirations(os.Args[2:])
	case "reconcile":
		code, err = runReconcile(os.Args[2:])
	case "db":
		err = runDB(os.Args[2:])
	case "cred":
		code, err = runCred(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
	os.Exit(code)
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  ob ledger  --snapshot DIR [--account NAME] [--asof YYYY-MM-DD] [--json]
  ob checkin --snapshot DIR [--asof YYYY-MM-DD] [--json]
  ob ticket  --snapshot DIR --account NAME --kind covered_call|acquisition_put|income_put --option INSTRUMENT_ID --limit PRICE [--qty N] [--json]
  ob book    --snapshot DIR [--into FILE | --write]   (--write splices into the book's own doc)
  ob snapshot --snapshot DIR            (check completeness; fingerprint when clean)
  ob diff    --from DIR --to DIR
  ob history plan --account NAME
  ob history add  --account NAME --observed RFC3339 (--created-since YYYY-MM-DD | --full) --pull PAGE1[,PAGE2...] [--pull ...] [--snapshot DIR]
  ob expirations --snapshot DIR [--bars FILE]   (list unresolved expirations, or record them from saved day bars)
  ob reconcile --snapshot DIR --pnl ACCOUNT=FILE [--pnl ...]   (ledger vs Robinhood realized option P&L; exits 1 on a mismatch)
  ob db import (--postgres DSN | --cloudsql INSTANCE/DATABASE/USER) (--from-repo DIR | --from-export DIR)
  ob db export (--postgres DSN | --cloudsql INSTANCE/DATABASE/USER) --to DIR
  ob db verify (--postgres DSN | --cloudsql INSTANCE/DATABASE/USER)
  ob db migrate (--postgres DSN | --cloudsql INSTANCE/DATABASE/USER) [--server-role ROLE]
  ob db check-append-only (--postgres DSN | --cloudsql INSTANCE/DATABASE/USER)   (tries UPDATE, DELETE, TRUNCATE; rolled back)
  ob db list   (--postgres DSN | --cloudsql INSTANCE/DATABASE/USER)
  ob db show   (--postgres DSN | --cloudsql INSTANCE/DATABASE/USER) --snapshot ID [--raw FILE ...]
  ob cred enroll robinhood|alpaca-paper --project ID   (sign in / paste the key; stores a new secret version)
  ob cred refresh robinhood --project ID [--force]     (refresh if under two days left; keeps at most two versions)
  ob cred status --project ID                         (version and expiry of each credential; never the secret)

every command also takes --book NAME (default ira-wheel; its config is books/NAME/{book,rules}.json) and
--config, --accounts and --rules to name a file explicitly.

ledger, checkin, ticket, book, diff and reconcile take --source files | postgres=DSN | cloudsql=INSTANCE/DATABASE/USER
(default files); with a store source, --snapshot (and --from/--to) name the snapshot id.`)
	os.Exit(2)
}

type common struct {
	bookFlags
	snap, asof, source string
	json               bool
}

func flags(name string, args []string, extra func(*flag.FlagSet)) common {
	var c common
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.StringVar(&c.snap, "snapshot", "", "snapshot directory (e.g. snapshots/2026-11-04)")
	fs.StringVar(&c.asof, "asof", "", "as-of date, YYYY-MM-DD (default: the snapshot id's session date)")
	c.register(fs)
	fs.StringVar(&c.source, "source", "files", "where the snapshot is read from: files | postgres=DSN | cloudsql=INSTANCE/DATABASE/USER (read-only)")
	fs.BoolVar(&c.json, "json", false, "emit JSON")
	if extra != nil {
		extra(fs)
	}
	fs.Parse(args)
	if c.snap == "" {
		usage()
	}
	asOf, err := resolveAsOf(c.snap, c.asof)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
	c.asof = asOf.Format("2006-01-02")
	return c
}

func load(c common) (*config.Book, *wheel.Book, error) {
	cfg, err := c.loadConfig()
	if err != nil {
		return nil, nil, err
	}
	asOf, err := resolveAsOf(c.snap, c.asof)
	if err != nil {
		return nil, nil, err
	}
	var names []string
	for _, a := range cfg.Accounts {
		names = append(names, a.Name)
	}
	at, err := atFor(c.source, c.snap)
	if err != nil {
		return nil, nil, err
	}
	at.Broker = cfg.Source()
	snap, err := snapshot.LoadAt(at, names)
	if err != nil {
		return nil, nil, err
	}
	bk, err := wheel.Assemble(cfg, snap, asOf)
	return cfg, bk, err
}

func emit(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// usd formats dollars to cents; see internal/format.
func usd(d decimal.Decimal) string { return format.USD(d) }

func runSnapshot(args []string) (int, error) {
	c := flags("snapshot", args, nil)
	if err := filesOnly(c.source, "snapshot"); err != nil {
		return 0, err
	}
	cfg, err := c.loadConfig()
	if err != nil {
		return 0, err
	}
	at := snapshot.OnDisk(c.snap)
	at.Broker = cfg.Source()
	ps := snapshot.CheckAt(at, report.AccountRefs(cfg))
	if c.json {
		if err := emit(ps); err != nil {
			return 0, err
		}
	}
	if len(ps) > 0 {
		if !c.json {
			report.SnapshotProblems(os.Stdout, c.snap, ps)
		}
		return 1, nil
	}
	if _, err := snapshot.ReadManifest(c.snap); err == nil {
		if !c.json {
			fmt.Printf("Snapshot %s is complete and matches its manifest.\n", c.snap)
		}
		return 0, nil
	}
	m, err := snapshot.WriteManifest(c.snap, time.Now())
	if err != nil {
		return 0, err
	}
	if !c.json {
		fmt.Printf("Snapshot %s is complete. Wrote manifest.json fingerprinting %d files; the snapshot is now read-only.\n", c.snap, len(m.Files))
	}
	return 0, nil
}

// pullFlags collects repeated --pull values; each is a comma-separated list
// of page files for one query.
type pullFlags []string

func (p *pullFlags) String() string     { return strings.Join(*p, " ") }
func (p *pullFlags) Set(v string) error { *p = append(*p, v); return nil }

// writableSnapshot refuses a --snapshot target that is already sealed. It runs
// before the log is touched, so a refusal leaves the log and the snapshot in
// step (B2).
func writableSnapshot(dir string) error {
	if dir == "" {
		return nil
	}
	if _, err := snapshot.ParseID(dir); err != nil {
		return err
	}
	if snapshot.Sealed(dir) {
		return fmt.Errorf("%s is sealed (it has manifest.json), so history add can't write into it; nothing was appended to the log. Run history add before ob snapshot, or start a new snapshot folder", dir)
	}
	return nil
}

func historyPath(account string) string { return filepath.FromSlash(layout.Log(account)) }

func runHistory(args []string) error {
	if len(args) == 0 {
		usage()
	}
	sub, args := args[0], args[1:]
	fs := flag.NewFlagSet("history "+sub, flag.ExitOnError)
	account := fs.String("account", "", "account folder name (roth, trad)")
	var observed, createdSince, snap string
	var full bool
	var pulls pullFlags
	if sub == "add" {
		fs.StringVar(&observed, "observed", "", "when the pull was made, RFC3339 (e.g. 2026-11-09T15:30:00Z)")
		fs.StringVar(&createdSince, "created-since", "", "the created_at_gte used for the window query (YYYY-MM-DD)")
		fs.BoolVar(&full, "full", false, "the pull is a full history (no created_at filter)")
		fs.Var(&pulls, "pull", "page files of one query, comma-separated; repeat for each query or single-order lookup")
		fs.StringVar(&snap, "snapshot", "", "write <snapshot>/<account>/history.json referencing the log after this add")
	}
	fs.Parse(args)
	if *account == "" {
		usage()
	}
	l, err := history.Load(historyPath(*account))
	if err != nil {
		return err
	}
	switch sub {
	case "plan":
		report.HistoryPlan(stdout, *account, l, fmt.Sprintf("Then: ob history add --account %s --observed <when> --created-since %s --pull <window pages> --pull <open pages> [--pull <lookup>…] --snapshot <dir>", *account, l.NextPlan().CreatedSince))
		return nil
	case "add":
		if observed == "" || len(pulls) == 0 || (createdSince == "" && !full) {
			usage()
		}
		if err := writableSnapshot(snap); err != nil {
			return err
		}
		obs, err := time.Parse(time.RFC3339, observed)
		if err != nil {
			return fmt.Errorf("--observed: %w", err)
		}
		in := history.AddInput{ObservedAt: obs, FullHistory: full}
		if createdSince != "" {
			if in.CreatedSince, err = time.Parse("2006-01-02", createdSince); err != nil {
				return fmt.Errorf("--created-since: %w", err)
			}
		}
		for _, p := range pulls {
			pl, err := history.ReadPull(strings.Split(p, ",")...)
			if err != nil {
				return err
			}
			in.Pulls = append(in.Pulls, pl)
		}
		res, err := l.Add(in)
		if err != nil {
			return err
		}
		fmt.Printf("%s: %d new orders, %d changed, %d unchanged; appended %d lines (log now %d).\n",
			*account, res.NewOrders, res.Changed, res.Unchanged, res.Appended, res.Lines)
		if snap != "" {
			sum, err := l.PrefixHash(res.Lines)
			if err != nil {
				return err
			}
			ref := history.Ref{Log: filepath.ToSlash(historyPath(*account)), Lines: res.Lines, SHA256: sum, ObservedAt: obs.UTC().Format(time.RFC3339)}
			b, _ := json.MarshalIndent(ref, "", "  ")
			p := filepath.Join(snap, filepath.FromSlash(layout.AccountFile(*account, layout.HistoryRef)))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(p, append(b, '\n'), 0o644); err != nil {
				return err
			}
			fmt.Printf("Wrote %s (replays the first %d lines).\n", p, res.Lines)
		}
		return nil
	}
	usage()
	return nil
}

var expirationsPath = filepath.FromSlash(layout.Expirations)

func runExpirations(args []string) error {
	var bars string
	c := flags("expirations", args, func(fs *flag.FlagSet) {
		fs.StringVar(&bars, "bars", "", "saved get_equity_historicals response (interval=day, adjustment_type=none) covering the expiration dates")
	})
	if err := filesOnly(c.source, "expirations"); err != nil {
		return err
	}
	_, bk, err := load(c)
	if err != nil {
		return err
	}
	todo := report.Unresolved(bk)
	if len(todo) == 0 {
		fmt.Fprint(stdout, report.NothingUnresolved)
		return nil
	}
	if bars == "" {
		report.ExpirationPlan(stdout, todo, fmt.Sprintf("Save it under history/bars/, then: ob expirations --snapshot %s --bars <file>", c.snap))
		return nil
	}
	barBytes, err := os.ReadFile(bars)
	if err != nil {
		return err
	}
	series, err := snapshot.DecodeBars(bars, barBytes)
	if err != nil {
		return err
	}
	recs, err := report.NewExpirations(stdout, todo, series, filepath.ToSlash(bars))
	if err != nil {
		return err
	}
	f := ledger.ExpirationsFile{Doc: ledger.ExpirationsDoc}
	if b, err := os.ReadFile(expirationsPath); err == nil {
		if err := json.Unmarshal(b, &f); err != nil {
			return fmt.Errorf("%s: %w", expirationsPath, err)
		}
	}
	f.Contracts = append(f.Contracts, recs...)
	b, _ := json.MarshalIndent(f, "", "  ")
	if err := os.MkdirAll(filepath.Dir(expirationsPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(expirationsPath, append(b, '\n'), 0o644); err != nil {
		return err
	}
	if _, err := ledger.LoadExpirations(expirationsPath); err != nil {
		return fmt.Errorf("wrote %s but it does not load back: %w", expirationsPath, err)
	}
	fmt.Fprintf(stdout, "Recorded %d outcome(s) in %s.\n", len(recs), expirationsPath)
	return nil
}
