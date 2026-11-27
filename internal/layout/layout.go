// Package layout is the one place that knows where things live in the repo
// layout (PLAN.md item 6 step 3a): the directories, the file names inside a
// snapshot, the order logs, the config files, and the slash paths built from
// them.
//
//	snapshots/<id>/<acct>/{equity_positions,option_positions,portfolio,history,…}.json
//	snapshots/<id>/<acct>/orders_pull/<page>.json
//	snapshots/<id>/options/<instrument>/{quote,instrument}.json
//	snapshots/<id>/{equity_quotes,option_quotes,expected,manifest}.json   PROVENANCE.md
//	history/<acct>/orders.jsonl     history/expirations.json     history/bars/<name>.json
//	reconcile/<id>/<acct>_realized_pnl.json
//	config/accounts.json            every account: identity, source, default book, execution modes
//	books/<book>/{book,rules}.json   a book: its strategy, accounts and rules
//
// The same layout is what every Source presents: the files on disk and the
// documents of the store both read as these paths, so a path is
// computed here and nowhere else. A test (TestNoPathLiterals) fails the build
// if a literal path or snapshot file name appears outside this package.
//
// Everything is a pure slash-path computation relative to a Source root. A
// caller that needs an operating-system path wraps one in filepath.FromSlash.
package layout

import "path"

// Top-level directories.
const (
	SnapshotsDir = "snapshots"
	HistoryDir   = "history"
	ReconcileDir = "reconcile"
	ConfigDir    = "config"
)

// File names inside a snapshot folder.
const (
	EquityPositions    = "equity_positions.json"
	OptionPositions    = "option_positions.json"
	Portfolio          = "portfolio.json"
	EquityQuotes       = "equity_quotes.json"
	OptionQuotes       = "option_quotes.json"
	OptionOrdersAll    = "option_orders_all.json"
	OptionOrdersFilled = "option_orders_filled.json"
	HistoryRef         = "history.json" // <acct>/history.json: the snapshot's position in the order log
	Manifest           = "manifest.json"
	Expected           = "expected.json"
	Provenance         = "PROVENANCE.md"
	GeneratedBook      = "wheel-book.generated.md" // a test golden, derived from the data
	OptionsDir         = "options"                 // options/<instrument>/…
	OrdersPullDir      = "orders_pull"             // <acct>/orders_pull/<page>.json
	QuoteFile          = "quote.json"
	InstrumentFile     = "instrument.json"
)

// Names of files outside a snapshot.
const (
	OrderLog        = "orders.jsonl"       // history/<acct>/orders.jsonl
	ExpirationsFile = "expirations.json"   // history/expirations.json
	BarsDir         = "bars"               // history/bars/<name>.json
	PnLSuffix       = "_realized_pnl.json" // reconcile/<id>/<acct>_realized_pnl.json
)

// Fixed paths from the repo root.
const (
	Expirations    = HistoryDir + "/" + ExpirationsFile
	AccountsConfig = ConfigDir + "/accounts.json"
)

// Books (PLAN.md item 6 step 3b): books/<book>/{book,rules}.json. A book picks a
// strategy and the accounts it covers; accounts.json holds the accounts themselves.
const (
	BooksDir      = "books"
	BookFileName  = "book.json"
	RulesFileName = "rules.json"
	// DefaultBook is the book every command uses unless --book (or a tool's book
	// parameter) says otherwise.
	DefaultBook = "ira-wheel"
)

// BookDir is a book's folder: books/<book>.
func BookDir(book string) string { return path.Join(BooksDir, book) }

// BookFile is a book's config: books/<book>/book.json.
func BookFile(book string) string { return path.Join(BooksDir, book, BookFileName) }

// RulesFile is a book's rules: books/<book>/rules.json.
func RulesFile(book string) string { return path.Join(BooksDir, book, RulesFileName) }

// Snapshot is a snapshot's folder: snapshots/<id>.
func Snapshot(id string) string { return path.Join(SnapshotsDir, id) }

// InSnapshot is a path inside a snapshot, from the repo root.
func InSnapshot(id string, rel ...string) string {
	return path.Join(append([]string{SnapshotsDir, id}, rel...)...)
}

// AccountFile is <acct>/<name>, relative to a snapshot folder.
func AccountFile(acct, name string) string { return path.Join(acct, name) }

// OrdersPull is a saved order-pull page, relative to a snapshot folder:
// <acct>/orders_pull/<page>.json.
func OrdersPull(acct, page string) string { return path.Join(acct, OrdersPullDir, page+".json") }

// OptionFile is a saved contract file, relative to a snapshot folder:
// options/<instrument>/<name>.
func OptionFile(instrument, name string) string { return path.Join(OptionsDir, instrument, name) }

// Log is an account's order log: history/<acct>/orders.jsonl.
func Log(acct string) string { return path.Join(HistoryDir, acct, OrderLog) }

// Bars is a saved day-bars response: history/bars/<name>.json.
func Bars(name string) string { return path.Join(HistoryDir, BarsDir, name+".json") }

// BarsFolder is the folder of saved day bars: history/bars.
func BarsFolder() string { return path.Join(HistoryDir, BarsDir) }

// PnL is a snapshot's saved realized-P&L response for an account:
// reconcile/<id>/<acct>_realized_pnl.json.
func PnL(id, acct string) string { return path.Join(ReconcileDir, id, PnLFile(acct)) }

// PnLFile is the file name of an account's realized-P&L response.
func PnLFile(acct string) string { return acct + PnLSuffix }

// ReconcileFolder is a snapshot's reconcile folder: reconcile/<id>.
func ReconcileFolder(id string) string { return path.Join(ReconcileDir, id) }

// OptionFolder is a saved contract's folder, relative to a snapshot folder:
// options/<instrument>.
func OptionFolder(instrument string) string { return path.Join(OptionsDir, instrument) }
