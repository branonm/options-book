package ledger_test

import (
	"encoding/json"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"testing"

	"github.com/branonm/options-book/internal/ledger"
	"github.com/branonm/options-book/internal/snapshot"
	"github.com/branonm/options-book/internal/store/storetest"
	"github.com/shopspring/decimal"
)

// TestAcceptanceSnapshots runs the ledger over every saved snapshot in the
// repo's snapshots/ folder that has an expected.json, as one subtest each.
// WHEEL_SNAPSHOT=<dir> runs a single snapshot from anywhere instead. Layout:
//
//	<dir>/expected.json
//	<dir>/<account>/option_orders_filled.json
//	<dir>/<account>/equity_positions.json
//	<dir>/<account>/option_positions.json   (optional)
func TestAcceptanceSnapshots(t *testing.T) {
	if d := os.Getenv("WHEEL_SNAPSHOT"); d != "" {
		at := snapshot.OnDisk(d)
		t.Run(path.Base(at.Dir), func(t *testing.T) { checkSnapshot(t, at) })
		return
	}
	for _, src := range storetest.Sources(t) {
		t.Run(src.Name, func(t *testing.T) {
			matches, err := fs.Glob(src.FS, "snapshots/*/expected.json")
			if err != nil {
				t.Fatal(err)
			}
			if len(matches) == 0 {
				t.Fatal("no snapshots with expected.json found")
			}
			for _, m := range matches {
				at := snapshot.At{FS: src.FS, Dir: path.Dir(m)}
				t.Run(path.Base(at.Dir), func(t *testing.T) { checkSnapshot(t, at) })
			}
		})
	}
}

func checkSnapshot(t *testing.T, at snapshot.At) {
	var want struct {
		AsOf     string `json:"asof"`
		Accounts map[string]struct {
			Realized    decimal.Decimal `json:"realized"`
			OpenCredit  decimal.Decimal `json:"open_credit"`
			BasisCredit decimal.Decimal `json:"basis_credit"`
			Blocks      map[string]struct {
				Shares    int64           `json:"shares"`
				Premium   decimal.Decimal `json:"premium"`
				Breakeven decimal.Decimal `json:"breakeven"`
			} `json:"blocks"`
		} `json:"accounts"`
		Combined struct {
			RealizedLegs int `json:"realized_legs"`
		} `json:"combined"`
	}
	b, err := fs.ReadFile(at.FS, path.Join(at.Dir, "expected.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatal(err)
	}

	legs := 0
	for name, w := range want.Accounts {
		snap, err := snapshot.LoadAt(at, []string{name})
		if err != nil {
			t.Fatal(err)
		}
		raw := snap.Accounts[name]
		in := ledger.Input{Account: name, AsOf: want.AsOf, CampaignStart: campaignStart(t, at), Orders: raw.Orders, Equity: raw.Equity, Expirations: snap.Expirations}
		l, err := ledger.Build(in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		legs += l.RealizedLegs
		for _, warn := range l.Warnings {
			t.Errorf("%s: warning: %s", name, warn)
		}
		if !l.Realized.Equal(w.Realized) {
			t.Errorf("%s realized = %s, want %s", name, l.Realized, w.Realized)
		}
		if !l.OpenCredit.Equal(w.OpenCredit) {
			t.Errorf("%s open credit = %s, want %s", name, l.OpenCredit, w.OpenCredit)
		}
		if !l.BasisCredit.Equal(w.BasisCredit) {
			t.Errorf("%s acquisition-put basis credit = %s, want %s", name, l.BasisCredit, w.BasisCredit)
		}
		for sym, wb := range w.Blocks {
			var got *ledger.Block
			for i := range l.Blocks {
				if l.Blocks[i].Symbol == sym {
					got = &l.Blocks[i]
				}
			}
			if got == nil {
				t.Errorf("%s %s: no block", name, sym)
				continue
			}
			if got.Shares != wb.Shares || !got.Premium.Equal(wb.Premium) {
				t.Errorf("%s %s: shares %d premium %s, want %d / %s", name, sym, got.Shares, got.Premium, wb.Shares, wb.Premium)
			}
			if be := got.Breakeven.Round(2); !be.Equal(wb.Breakeven) {
				t.Errorf("%s %s: breakeven %s, want %s", name, sym, be, wb.Breakeven)
			}
		}
	}
	if legs != want.Combined.RealizedLegs {
		t.Errorf("combined realized legs = %d, want %d", legs, want.Combined.RealizedLegs)
	}
}

// campaignStart is the IRA wheel book's campaign_start, read from the repo
// root on disk (the imported stores hold no books/), so a synthetic root
// (OB_TEST_ROOT) brings its own.
func campaignStart(t *testing.T, _ snapshot.At) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(storetest.Root(t), "books", "ira-wheel", "book.json"))
	if err != nil {
		t.Fatal(err)
	}
	var book struct {
		CampaignStart string `json:"campaign_start"`
	}
	if err := json.Unmarshal(b, &book); err != nil || book.CampaignStart == "" {
		t.Fatalf("books/ira-wheel/book.json: campaign_start: %v", err)
	}
	return book.CampaignStart
}
