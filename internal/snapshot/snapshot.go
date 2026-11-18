// Package snapshot loads a saved snapshot directory:
//
//	<dir>/equity_quotes.json
//	<dir>/<account>/history.json               (preferred: replay of history/<account>/orders.jsonl)
//	<dir>/<account>/option_orders_all.json     (a full saved history, every state)
//	<dir>/<account>/option_orders_filled.json  (fallback: fills only)
//	<dir>/<account>/equity_positions.json
//	<dir>/<account>/option_positions.json
//
// Every read goes through an At: a read-only view of the repo layout (an
// fs.FS rooted where snapshots/, history/ and reconcile/ live) plus the
// snapshot's slash path inside it. OnDisk is the files Source; the
// Firestore Source (internal/store) presents the same layout from documents.
package snapshot

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/branonm/options-book/internal/layout"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/shopspring/decimal"

	"github.com/branonm/options-book/internal/broker"
	_ "github.com/branonm/options-book/internal/broker/alpaca"    // the paper source (item 6 step 4)
	_ "github.com/branonm/options-book/internal/broker/robinhood" // registers the adapter DefaultBroker names
	"github.com/branonm/options-book/internal/history"
	"github.com/branonm/options-book/internal/ledger"
)

// DefaultBroker names the adapter snapshots are decoded with. Every snapshot
// in the repo is Robinhood's; config/accounts.json (item 6 step 3) will name the
// source per account, and this goes away.
const DefaultBroker = "robinhood"

// Decoder is the broker decoder snapshots are read with.
func Decoder() (broker.Decoder, error) { return broker.Lookup(DefaultBroker) }

// read reads one saved response from the Source and decodes it. A missing
// file is fs.ErrNotExist.
func read[T any](at At, p string, dec func(name string, b []byte) (T, error)) (T, error) {
	b, err := fs.ReadFile(at.FS, p)
	if err != nil {
		var z T
		return z, err
	}
	return dec(p, b)
}

type Account struct {
	Name            string
	Orders          []broker.Order
	OrdersAllStates bool // false when only filled orders were saved (open orders unknown)
	Equity          []broker.EquityPosition
	OptionPositions []broker.OptionPosition
	HasPositions    bool
	Portfolio       *broker.Balance // nil when portfolio.json is absent
}

type Snapshot struct {
	Dir          string
	Accounts     map[string]*Account
	Quotes       map[string]decimal.Decimal // last trade price
	HasQuote     bool
	QuoteTime    string                        // latest equity quote timestamp
	OptionQuotes map[string]broker.OptionQuote // by instrument id, from option_quotes.json
	// Expirations are recorded expiration outcomes from the repo's history/expirations.json.
	Expirations map[string]ledger.Expiration
}

// At locates a snapshot in a Source. FS is a read-only view rooted at the
// repository (snapshots/<id>/…, history/…, reconcile/…); Dir is the
// snapshot's slash path inside it, e.g. "snapshots/2026-11-07".
type At struct {
	FS  fs.FS
	Dir string
	// Broker names the adapter whose responses the snapshot holds; "" is
	// DefaultBroker (Robinhood). A book sets it from its accounts' source.
	Broker string
}

// decoder is the decoder for this snapshot's broker.
// Decoder is the decoder for this snapshot's broker ("" is the default).
func (a At) Decoder() (broker.Decoder, error) { return a.decoder() }

func (a At) decoder() (broker.Decoder, error) {
	if a.Broker == "" {
		return Decoder()
	}
	return broker.Lookup(a.Broker)
}

// OnDisk is the files Source for a snapshot folder: the repo root is two
// levels up (snapshots/<id>), as RepoRoot has always assumed.
func OnDisk(dir string) At {
	dir = filepath.Clean(dir)
	root := RepoRoot(dir)
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		rel = filepath.Join(filepath.Base(filepath.Dir(dir)), filepath.Base(dir))
	}
	return At{FS: os.DirFS(root), Dir: filepath.ToSlash(rel)}
}

func (a At) file(parts ...string) string { return path.Join(append([]string{a.Dir}, parts...)...) }

// ID parses the snapshot id from the view's path.
func (a At) ID() (ID, error) { return ParseID(path.Base(a.Dir)) }

// Load reads the named accounts from a snapshot folder on disk.
func Load(dir string, accounts []string) (*Snapshot, error) {
	s, err := LoadAt(OnDisk(dir), accounts)
	if s != nil {
		s.Dir = dir
	}
	return s, err
}

// LoadAt reads the named accounts. Order history and equity positions are
// required; option positions and quotes are optional, and callers check the
// Has* flags before relying on them.
func LoadAt(at At, accounts []string) (*Snapshot, error) {
	s := &Snapshot{Dir: at.Dir, Accounts: map[string]*Account{}, Quotes: map[string]decimal.Decimal{}, OptionQuotes: map[string]broker.OptionQuote{}}
	dec, err := at.decoder()
	if err != nil {
		return nil, err
	}
	for _, name := range accounts {
		a := &Account{Name: name}
		orders, all, err := loadOrders(at, dec, name)
		if err != nil {
			return nil, err
		}
		a.Orders, a.OrdersAllStates = orders, all

		eq, err := read(at, at.file(name, layout.EquityPositions), dec.EquityPositions)
		if err != nil {
			return nil, err
		}
		a.Equity = eq

		switch ops, err := read(at, at.file(name, layout.OptionPositions), dec.OptionPositions); {
		case err == nil:
			a.OptionPositions, a.HasPositions = ops, true
		case !errors.Is(err, fs.ErrNotExist):
			return nil, err
		}
		switch bal, err := read(at, at.file(name, layout.Portfolio), dec.Balance); {
		case err == nil:
			a.Portfolio = &bal
		case !errors.Is(err, fs.ErrNotExist):
			return nil, err
		}
		s.Accounts[name] = a
	}

	switch qs, err := read(at, at.file(layout.EquityQuotes), dec.EquityQuotes); {
	case err == nil:
		s.HasQuote = true
		for _, q := range qs {
			p, err := decimal.NewFromString(q.Last.Text)
			if err != nil {
				return nil, fmt.Errorf("quote %s: %w", q.Symbol, err)
			}
			s.Quotes[q.Symbol] = p
			if q.LastTradeAt > s.QuoteTime {
				s.QuoteTime = q.LastTradeAt
			}
		}
	case !errors.Is(err, fs.ErrNotExist):
		return nil, err
	}
	exps, err := ledger.LoadExpirationsFS(at.FS, ExpirationsFile)
	if err != nil {
		return nil, err
	}
	s.Expirations = exps

	switch oqs, err := read(at, at.file(layout.OptionQuotes), dec.OptionQuotes); {
	case err == nil:
		for _, q := range oqs {
			s.OptionQuotes[q.InstrumentID] = q
		}
	case !errors.Is(err, fs.ErrNotExist):
		return nil, err
	}
	return s, nil
}

// ExpirationsFile is where recorded expiration outcomes live in a Source.
const ExpirationsFile = layout.Expirations

// Option is one contract's saved quote and instrument record, stored under
// <dir>/options/<instrument_id>/{quote.json,instrument.json}.
type Option struct {
	Quote      broker.OptionQuote
	Instrument broker.Instrument
}

// LoadOption reads a contract's quote and instrument from a snapshot on disk.
func LoadOption(dir, instrumentID string) (*Option, error) {
	return LoadOptionAt(OnDisk(dir), instrumentID)
}

// LoadOptionAt reads a contract's quote and instrument from the snapshot.
func LoadOptionAt(at At, instrumentID string) (*Option, error) {
	base := at.file(layout.OptionFolder(instrumentID))
	dec, err := at.decoder()
	if err != nil {
		return nil, err
	}
	qs, err := read(at, path.Join(base, layout.QuoteFile), dec.OptionQuotes)
	if err != nil {
		return nil, err
	}
	is, err := read(at, path.Join(base, layout.InstrumentFile), dec.Instruments)
	if err != nil {
		return nil, err
	}
	if len(qs) != 1 || len(is) != 1 {
		return nil, fmt.Errorf("%s: want exactly one quote and one instrument", base)
	}
	o := &Option{Quote: qs[0], Instrument: is[0]}
	o.JoinOpenInterest()
	if o.Quote.InstrumentID != instrumentID || o.Instrument.ID != instrumentID {
		return nil, fmt.Errorf("%s: files are for a different instrument", base)
	}
	return o, nil
}

// loadOrders reads an account's orders, in order of preference:
//  1. <account>/history.json — replay the first N lines of the repo's order
//     history log, verified against the recorded fingerprint;
//  2. <account>/option_orders_all.json — a full saved history (every state);
//  3. <account>/option_orders_filled.json — fills only (open orders unknown).
func loadOrders(at At, dec broker.Decoder, name string) ([]broker.Order, bool, error) {
	refPath := at.file(name, layout.HistoryRef)
	if b, err := fs.ReadFile(at.FS, refPath); err == nil {
		var ref history.Ref
		if err := json.Unmarshal(b, &ref); err != nil {
			return nil, false, fmt.Errorf("%s: %w", refPath, err)
		}
		orders, err := ReplayRefFS(at.FS, dec, ref)
		if err != nil {
			return nil, false, fmt.Errorf("%s: %w", refPath, err)
		}
		return orders, true, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, false, err
	}
	switch orders, err := read(at, at.file(name, layout.OptionOrdersAll), dec.Orders); {
	case err == nil:
		return orders, true, nil
	case errors.Is(err, fs.ErrNotExist):
		orders, err := read(at, at.file(name, layout.OptionOrdersFilled), dec.Orders)
		if err != nil {
			return nil, false, err
		}
		return orders, false, nil
	default:
		return nil, false, err
	}
}

// RepoRoot is the repository root for a snapshot folder (snapshots/<date>).
func RepoRoot(dir string) string { return filepath.Join(dir, "..", "..") }

// ReplayRef reconstructs the orders a snapshot on disk saw from the history log.
func ReplayRef(dir string, ref history.Ref) ([]broker.Order, error) {
	dec, err := Decoder()
	if err != nil {
		return nil, err
	}
	return ReplayRefFS(OnDisk(dir).FS, dec, ref)
}

// ReplayRefFS reconstructs the orders a snapshot saw from the history log in
// a Source, verifying the prefix fingerprint, decoding each order record with dec.
func ReplayRefFS(fsys fs.FS, dec broker.Decoder, ref history.Ref) ([]broker.Order, error) {
	if ref.Log == "" || ref.Lines <= 0 || ref.SHA256 == "" {
		return nil, fmt.Errorf("history reference needs log, lines and sha256")
	}
	l, err := history.LoadFSFor(fsys, ref.Log, dec.Name())
	if err != nil {
		return nil, err
	}
	sum, err := l.PrefixHash(ref.Lines)
	if err != nil {
		return nil, err
	}
	if sum != ref.SHA256 {
		return nil, fmt.Errorf("the first %d lines of %s no longer match this snapshot's fingerprint; the history log was rewritten", ref.Lines, ref.Log)
	}
	recs, err := l.OrderRecords(ref.Lines)
	if err != nil {
		return nil, err
	}
	out := make([]broker.Order, 0, len(recs))
	for _, raw := range recs {
		o, err := dec.OrderRecord(ref.Log, raw)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}

// DecodeRealizedPnL decodes a saved get_realized_pnl response with the
// snapshot decoder; name labels errors.
func DecodeRealizedPnL(name string, b []byte) (broker.RealizedPnL, error) {
	dec, err := Decoder()
	if err != nil {
		return broker.RealizedPnL{}, err
	}
	return dec.RealizedPnL(name, b)
}

// DecodeBars decodes a saved day-bars response with the snapshot decoder.
func DecodeBars(name string, b []byte) ([]broker.BarSeries, error) {
	dec, err := Decoder()
	if err != nil {
		return nil, err
	}
	return dec.Bars(name, b)
}

// JoinOpenInterest fills the quote's open interest from the instrument. A
// source that keeps it on the contract (Alpaca) leaves it off the quote, so a
// ticket would read 0; a quote that carries its own (Robinhood) is never
// overridden.
func (o *Option) JoinOpenInterest() {
	if o.Quote.OpenInterest == 0 && o.Instrument.OpenInterest > 0 {
		o.Quote.OpenInterest = o.Instrument.OpenInterest
	}
}
