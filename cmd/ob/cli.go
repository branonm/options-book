package main

// The figure-producing commands. Each parses its flags and calls
// internal/report, which the ob-mcp server calls too, so a tool's output
// is the CLI's output byte for byte. stdout is a variable so tests can
// capture it.

import (
	"flag"
	"fmt"
	"github.com/branonm/options-book/internal/layout"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/branonm/options-book/internal/config"
	"github.com/branonm/options-book/internal/render"
	"github.com/branonm/options-book/internal/report"
)

var stdout io.Writer = os.Stdout

func resolveAsOf(dir, asof string) (time.Time, error) { return report.ResolveAsOf(dir, asof) }

// prepare loads the config and locates the snapshot for a command.
func prepare(c common) (*config.Book, time.Time, error) {
	cfg, err := c.loadConfig()
	if err != nil {
		return nil, time.Time{}, err
	}
	asOf, err := resolveAsOf(c.snap, c.asof)
	return cfg, asOf, err
}

func runLedger(args []string) error {
	var only string
	c := flags("ledger", args, func(fs *flag.FlagSet) { fs.StringVar(&only, "account", "", "one account (default: all)") })
	cfg, asOf, err := prepare(c)
	if err != nil {
		return err
	}
	at, err := atFor(c.source, c.snap)
	if err != nil {
		return err
	}
	return report.Ledger(stdout, cfg, at, asOf, only, c.json)
}

func runCheckin(args []string) (int, error) {
	c := flags("checkin", args, nil)
	r, err := c.loadRules()
	if err != nil {
		return 0, err
	}
	cfg, asOf, err := prepare(c)
	if err != nil {
		return 0, err
	}
	at, err := atFor(c.source, c.snap)
	if err != nil {
		return 0, err
	}
	return report.Checkin(stdout, cfg, r, at, asOf, c.json)
}

func runTicket(args []string) (int, error) {
	var req report.TicketRequest
	c := flags("ticket", args, func(fs *flag.FlagSet) {
		fs.StringVar(&req.Account, "account", "", "account name (e.g. roth)")
		fs.StringVar(&req.Kind, "kind", "", "covered_call | acquisition_put | income_put")
		fs.StringVar(&req.Option, "option", "", "option instrument id; quote and instrument must be saved under <snapshot>/options/<id>/")
		fs.StringVar(&req.Limit, "limit", "", "limit price per share")
		fs.Int64Var(&req.Qty, "qty", 0, "contracts (default: every free lot for a call, 1 for a put)")
	})
	if req.Account == "" || req.Kind == "" || req.Option == "" || req.Limit == "" {
		usage()
	}
	r, err := c.loadRules()
	if err != nil {
		return 0, err
	}
	cfg, asOf, err := prepare(c)
	if err != nil {
		return 0, err
	}
	at, err := atFor(c.source, c.snap)
	if err != nil {
		return 0, err
	}
	return report.Ticket(stdout, cfg, r, at, asOf, req, c.json)
}

// bookLabel names the snapshot in the wheel book's provenance line: the
// folder as given on disk, or "snapshots/<id>" from a store, where the store
// presents it — so the book is byte-identical whichever source it came from.
func bookLabel(source, snap string) string {
	if source != "" && source != "files" {
		return layout.Snapshot(filepath.Base(filepath.Clean(snap)))
	}
	return snap
}

func runBook(args []string) error {
	var into string
	var write bool
	c := flags("book", args, func(fs *flag.FlagSet) {
		fs.StringVar(&into, "into", "", "splice the generated section into this Markdown file, between its BEGIN/END GENERATED markers")
		fs.BoolVar(&write, "write", false, "splice it into the book's own document (book.json's doc), instead of --into FILE")
	})
	r, err := c.loadRules()
	if err != nil {
		return err
	}
	cfg, asOf, err := prepare(c)
	if err != nil {
		return err
	}
	at, err := atFor(c.source, c.snap)
	if err != nil {
		return err
	}
	if write {
		switch {
		case into != "":
			return fmt.Errorf("give --write or --into FILE, not both")
		case cfg.Doc == "":
			return fmt.Errorf("the %s book has no doc; add one to its book.json or use --into FILE", cfg.Name)
		}
		into = filepath.FromSlash(cfg.Doc)
	}
	if into == "" {
		return report.Book(stdout, cfg, r, at, asOf, bookLabel(c.source, c.snap))
	}
	var b strings.Builder
	if err := report.Book(&b, cfg, r, at, asOf, bookLabel(c.source, c.snap)); err != nil {
		return err
	}
	doc, err := os.ReadFile(into)
	if err != nil {
		return err
	}
	out, err := render.Splice(string(doc), b.String())
	if err != nil {
		return fmt.Errorf("%s: %w", into, err)
	}
	return os.WriteFile(into, []byte(out), 0o644)
}

func runDiff(args []string) error {
	fs := flag.NewFlagSet("diff", flag.ExitOnError)
	from := fs.String("from", "", "earlier snapshot folder")
	to := fs.String("to", "", "later snapshot folder")
	var bf bookFlags
	bf.register(fs)
	source := fs.String("source", "files", "where the snapshots are read from: files | emulator=HOST:PORT | firestore=PROJECT (read-only)")
	fs.Parse(args)
	if *from == "" || *to == "" {
		usage()
	}
	if err := report.CheckOrder(*from, *to); err != nil {
		return err
	}
	cfg, err := bf.loadConfig()
	if err != nil {
		return err
	}
	r, err := bf.loadRules()
	if err != nil {
		return err
	}
	a, err := atFor(*source, *from)
	if err != nil {
		return err
	}
	b, err := atFor(*source, *to)
	if err != nil {
		return err
	}
	return report.Diff(stdout, cfg, r, a, b)
}

func runReconcile(args []string) (int, error) {
	var pnlFlags pullFlags
	c := flags("reconcile", args, func(fs *flag.FlagSet) {
		fs.Var(&pnlFlags, "pnl", "ACCOUNT=FILE: a saved get_realized_pnl(asset_classes=[option], start_date, end_date) response")
	})
	if len(pnlFlags) == 0 {
		usage()
	}
	cfg, asOf, err := prepare(c)
	if err != nil {
		return 0, err
	}
	at, err := atFor(c.source, c.snap)
	if err != nil {
		return 0, err
	}
	var pnls []report.PnL
	for _, p := range pnlFlags {
		acct, file, ok := strings.Cut(p, "=")
		if !ok {
			return 0, fmt.Errorf("--pnl %q: want ACCOUNT=FILE", p)
		}
		resp, err := loadPnL(c.source, file)
		if err != nil {
			return 0, err
		}
		pnls = append(pnls, report.PnL{Account: acct, Resp: resp})
	}
	return report.Reconcile(stdout, cfg, at, asOf, pnls, c.json)
}
