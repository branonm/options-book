package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/branonm/options-book/internal/layout"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/branonm/options-book/internal/history"
)

// AccountRef names an account for the check: its folder and a label for messages.
type AccountRef struct {
	Name  string // folder name, e.g. "roth"
	Label string // e.g. "Roth IRA ••0001"
}

// Problem is one thing wrong with a snapshot, and the call that fixes it.
type Problem struct {
	File    string `json:"file"`
	Problem string `json:"problem"`
	Fix     string `json:"fix"`
}

// Manifest fingerprints a snapshot's raw inputs so later edits are detectable.
type Manifest struct {
	CreatedAt string            `json:"created_at"`
	Files     map[string]string `json:"sha256"` // relative path → hex digest
}

const manifestName = layout.Manifest

// rawInput reports whether a snapshot file is a saved broker response (as
// opposed to provenance notes, expectations or generated output).
func rawInput(rel string) bool {
	base := filepath.Base(rel)
	switch base {
	case manifestName, layout.Expected:
		return false
	}
	return strings.HasSuffix(base, ".json")
}

// Check validates a snapshot directory: every required response is present
// and readable, quotes cover every holding and open contract, and — when a
// manifest exists — no raw input has changed since it was written.
func Check(dir string, accounts []AccountRef) []Problem { return CheckAt(OnDisk(dir), accounts) }

// CheckAt is Check over any Source.
func CheckAt(at At, accounts []AccountRef) []Problem {
	var ps []Problem
	add := func(file, problem, fix string) { ps = append(ps, Problem{file, problem, fix}) }
	dec, err := at.decoder()
	if err != nil {
		return []Problem{{File: "", Problem: err.Error(), Fix: "register the broker adapter"}}
	}

	held := map[string]bool{}
	openIDs := map[string]string{} // option_id → label
	for _, a := range accounts {
		f := func(name string) string { return filepath.Join(a.Name, name) }
		// report adds the problem a failed read or decode is (missing, or its error).
		report := func(name string, err error, fix string) bool {
			switch {
			case errors.Is(err, fs.ErrNotExist):
				add(f(name), "missing", fix)
				return false
			case err != nil:
				add(f(name), err.Error(), fix)
				return false
			}
			return true
		}
		if b, err := fs.ReadFile(at.FS, at.file(a.Name, layout.HistoryRef)); err == nil {
			var ref history.Ref
			if err := json.Unmarshal(b, &ref); err != nil {
				add(f(layout.HistoryRef), err.Error(), "rewrite it with `ob history add --snapshot`")
			} else if orders, err := ReplayRefFS(at.FS, dec, ref); err != nil {
				add(f(layout.HistoryRef), err.Error(), "restore "+layout.Log(a.Name)+" from git; the log is append-only")
			} else if len(orders) == 0 && ref.Lines > 0 {
				add(f(layout.HistoryRef), "replays to no orders", "re-run `ob history add` with the pull files")
			}
		} else {
			os, err := read(at, at.file(a.Name, layout.OptionOrdersAll), dec.Orders)
			if report(layout.OptionOrdersAll, err, fmt.Sprintf("get_option_orders(%s) per `ob history plan --account %s`, then `ob history add --snapshot`", a.Label, a.Name)) && len(os) == 0 {
				add(f(layout.OptionOrdersAll), "no orders in the response", "re-pull; an empty history is almost certainly a failed call")
			}
		}
		ps, err := read(at, at.file(a.Name, layout.EquityPositions), dec.EquityPositions)
		if report(layout.EquityPositions, err, fmt.Sprintf("get_equity_positions(%s)", a.Label)) {
			for _, p := range ps {
				held[p.Symbol] = true
			}
		}
		ops, err := read(at, at.file(a.Name, layout.OptionPositions), dec.OptionPositions)
		if report(layout.OptionPositions, err, fmt.Sprintf("get_option_positions(%s, nonzero=true)", a.Label)) {
			for _, p := range ops {
				openIDs[p.InstrumentID] = fmt.Sprintf("%s %s %s", a.Label, p.Symbol, p.Expiry)
			}
		}
		bal, err := read(at, at.file(a.Name, layout.Portfolio), dec.Balance)
		if report(layout.Portfolio, err, fmt.Sprintf("get_portfolio(%s)", a.Label)) && bal.TotalValue.Text == "" {
			add(f(layout.Portfolio), "no total_value", fmt.Sprintf("re-pull get_portfolio(%s)", a.Label))
		}
	}

	// Equity quotes: one per held symbol, active, all from one session.
	syms := sortedKeys(held)
	switch eqs, err := read(at, at.file(layout.EquityQuotes), dec.EquityQuotes); {
	case errors.Is(err, fs.ErrNotExist) && len(syms) == 0:
		// Nothing is held, so there is nothing to quote: a new account (B9).
	case errors.Is(err, fs.ErrNotExist):
		add(layout.EquityQuotes, "missing", fmt.Sprintf("get_equity_quotes(%s)", strings.Join(syms, ", ")))
	case err != nil:
		add(layout.EquityQuotes, err.Error(), "re-pull get_equity_quotes")
	default:
		have := map[string]bool{}
		days := map[string]bool{}
		for _, q := range eqs {
			have[q.Symbol] = true
			if q.State != "" && q.State != "active" {
				add(layout.EquityQuotes, fmt.Sprintf("%s quote state is %q", q.Symbol, q.State), "check the symbol before using its price")
			}
			if t, err := time.Parse(time.RFC3339Nano, q.LastTradeAt); err == nil {
				days[t.UTC().Format("2006-01-02")] = true
			} else {
				add(layout.EquityQuotes, fmt.Sprintf("%s has no readable venue_last_trade_time", q.Symbol), "re-pull get_equity_quotes")
			}
		}
		var missing []string
		for _, s := range syms {
			if !have[s] {
				missing = append(missing, s)
			}
		}
		if len(missing) > 0 {
			add(layout.EquityQuotes, "no quote for "+strings.Join(missing, ", "), "get_equity_quotes("+strings.Join(missing, ", ")+") and merge")
		}
		if len(days) > 1 {
			add(layout.EquityQuotes, "quotes span sessions "+strings.Join(sortedKeys(days), ", "), "re-pull every symbol in one call")
		}
	}

	// Option quotes: one per open contract.
	if len(openIDs) > 0 {
		switch oqs, err := read(at, at.file(layout.OptionQuotes), dec.OptionQuotes); {
		case errors.Is(err, fs.ErrNotExist):
			add(layout.OptionQuotes, "missing", "get_option_quotes(["+strings.Join(sortedKeys(openIDs), ", ")+"])")
		case err != nil:
			add(layout.OptionQuotes, err.Error(), "re-pull get_option_quotes")
		default:
			have := map[string]bool{}
			for _, q := range oqs {
				have[q.InstrumentID] = true
			}
			var missing, labels []string
			for _, id := range sortedKeys(openIDs) {
				if !have[id] {
					missing = append(missing, id)
					labels = append(labels, openIDs[id])
				}
			}
			if len(missing) > 0 {
				add(layout.OptionQuotes, "no quote for "+strings.Join(labels, "; "), "get_option_quotes(["+strings.Join(missing, ", ")+"]) and merge")
			}
		}
	}

	// Manifest: any raw input changed since it was written is a problem.
	m, err := ReadManifestAt(at)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		add(manifestName, err.Error(), "restore it from git")
	default:
		current, err := HashInputs(at)
		if err != nil {
			add(manifestName, err.Error(), "")
			break
		}
		for rel, sum := range m.Files {
			switch cur, ok := current[rel]; {
			case !ok:
				add(rel, "listed in the manifest but missing", "restore it from git")
			case cur != sum:
				add(rel, "changed since the manifest was written", "restore it from git; a snapshot is immutable once checked")
			}
		}
		for rel := range current {
			if _, ok := m.Files[rel]; !ok {
				add(rel, "added after the manifest was written", "start a new snapshot folder instead of editing a checked one")
			}
		}
	}
	sort.SliceStable(ps, func(i, j int) bool { return ps[i].File < ps[j].File })
	return ps
}

// WriteManifest fingerprints the snapshot's raw inputs. It refuses to
// overwrite an existing manifest.
func WriteManifest(dir string, now time.Time) (*Manifest, error) {
	p := filepath.Join(dir, manifestName)
	if _, err := os.Stat(p); err == nil {
		return nil, fmt.Errorf("%s already exists; a checked snapshot is immutable", p)
	}
	files, err := HashInputs(OnDisk(dir))
	if err != nil {
		return nil, err
	}
	m := &Manifest{CreatedAt: now.UTC().Format(time.RFC3339), Files: files}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return m, os.WriteFile(p, append(b, '\n'), 0o644)
}

// ReadManifest loads <dir>/manifest.json.
func ReadManifest(dir string) (*Manifest, error) { return ReadManifestAt(OnDisk(dir)) }

// ReadManifestAt loads the snapshot's manifest.json from a Source.
func ReadManifestAt(at At) (*Manifest, error) {
	b, err := fs.ReadFile(at.FS, at.file(manifestName))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("manifest.json: %w", err)
	}
	return &m, nil
}

// HashInputs fingerprints every raw input in a snapshot: relative slash path → sha256 hex.
func HashInputs(at At) (map[string]string, error) {
	out := map[string]string{}
	err := fs.WalkDir(at.FS, at.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel := strings.TrimPrefix(p, at.Dir+"/")
		if !rawInput(rel) {
			return nil
		}
		b, err := fs.ReadFile(at.FS, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		out[rel] = hex.EncodeToString(sum[:])
		return nil
	})
	return out, err
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
