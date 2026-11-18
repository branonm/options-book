package layout

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Each path is exactly what the literal it replaces produced.
func TestPaths(t *testing.T) {
	for name, c := range map[string][2]string{
		"snapshot":        {Snapshot("2026-11-17"), "snapshots/2026-11-17"},
		"snapshot timed":  {Snapshot("2026-11-13T1151"), "snapshots/2026-11-13T1151"},
		"in snapshot":     {InSnapshot("2026-11-17", "roth", "portfolio.json"), "snapshots/2026-11-17/roth/portfolio.json"},
		"account file":    {AccountFile("roth", HistoryRef), "roth/history.json"},
		"orders pull":     {OrdersPull("trad", "lookup_6aeb4f0a"), "trad/orders_pull/lookup_6aeb4f0a.json"},
		"option quote":    {OptionFile("d85090f6", QuoteFile), "options/d85090f6/quote.json"},
		"option instr":    {OptionFile("d85090f6", InstrumentFile), "options/d85090f6/instrument.json"},
		"log":             {Log("roth"), "history/roth/orders.jsonl"},
		"expirations":     {Expirations, "history/expirations.json"},
		"bars":            {Bars("2026-11-08_xle-jets-kre"), "history/bars/2026-11-08_xle-jets-kre.json"},
		"bars folder":     {BarsFolder(), "history/bars"},
		"pnl":             {PnL("2026-11-07", "roth"), "reconcile/2026-11-07/roth_realized_pnl.json"},
		"pnl file":        {PnLFile("trad"), "trad_realized_pnl.json"},
		"reconcile":       {ReconcileFolder("2026-11-07"), "reconcile/2026-11-07"},
		"accounts config": {AccountsConfig, "config/accounts.json"},
		"book dir":        {BookDir("ira-wheel"), "books/ira-wheel"},
		"book file":       {BookFile("ira-wheel"), "books/ira-wheel/book.json"},
		"rules file":      {RulesFile("ira-wheel"), "books/ira-wheel/rules.json"},
		"default book":    {DefaultBook, "ira-wheel"},
		"manifest in dir": {InSnapshot("2026-11-17", Manifest), "snapshots/2026-11-17/manifest.json"},
	} {
		if c[0] != c[1] {
			t.Errorf("%s: %q, want %q", name, c[0], c[1])
		}
	}
}

// pathLiteral says whether a string literal is a repo path or a snapshot
// file name that belongs to this package. Prose that merely mentions a file
// ("name a file such as roth/portfolio.json") is not: only a literal that
// *is* a path, or *starts* one, counts.
func pathLiteral(s string) bool {
	for _, p := range []string{"snapshots/", "history/", "reconcile/", "config/", "books/", "options/"} {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	switch s {
	case EquityPositions, OptionPositions, Portfolio, EquityQuotes, OptionQuotes, OptionOrdersAll, OptionOrdersFilled,
		HistoryRef, Manifest, Expected, Provenance, GeneratedBook, QuoteFile, InstrumentFile, OrderLog, ExpirationsFile, PnLSuffix,
		"/" + HistoryRef:
		return true
	}
	return false
}

// TestNoPathLiterals is 3a's guard: outside this package, no non-test Go file
// spells a repo path or a snapshot file name. A path is computed here, so a
// layout change is one file.
func TestNoPathLiterals(t *testing.T) {
	root := filepath.Join("..", "..")
	var hits []string
	files := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".idea", "bin", "snapshots", "history", "reconcile", "infra", "docs", "spike":
				return fs.SkipDir
			}
			if filepath.ToSlash(rel) == "internal/layout" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files++
		fset := token.NewFileSet()
		_ = fset
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err == nil && pathLiteral(s) {
				hits = append(hits, filepath.ToSlash(rel)+": "+lit.Value)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 50 {
		t.Fatalf("only %d Go files checked; the walk is broken", files)
	}
	sort.Strings(hits)
	for _, h := range hits {
		t.Errorf("path literal outside internal/layout: %s", h)
	}
}
