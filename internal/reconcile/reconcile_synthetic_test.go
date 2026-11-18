package reconcile

import (
	"github.com/branonm/options-book/internal/layout"
	"io/fs"
	"testing"
	"time"

	"github.com/branonm/options-book/internal/config"
	"github.com/branonm/options-book/internal/snapshot"
	"github.com/branonm/options-book/internal/store/storetest"
	"github.com/branonm/options-book/internal/strategy/wheel"
)

// The 2026-02-13 synthetic ledger against Robinhood's own option P&L for 1/05–2/13 (both $300 and $100, no assigned calls).
func TestReconcile0213(t *testing.T) {
	root := storetest.Root(t)
	cfg, err := config.Load(root, layout.DefaultBook)
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range storetest.Sources(t) {
		t.Run(src.Name, func(t *testing.T) { reconcile0213(t, src.FS, cfg) })
	}
}

func reconcile0213(t *testing.T, fsys fs.FS, cfg *config.Book) {
	snap, err := snapshot.LoadAt(snapshot.At{FS: fsys, Dir: "snapshots/2026-02-13"}, []string{"roth", "trad"})
	if err != nil {
		t.Fatal(err)
	}
	asOf, _ := time.Parse("2006-01-02", "2026-02-13")
	bk, err := wheel.Assemble(cfg, snap, asOf)
	if err != nil {
		t.Fatal(err)
	}
	for acct, want := range map[string]string{"roth": "0", "trad": "0"} {
		b, err := fs.ReadFile(fsys, "reconcile/2026-02-13/"+acct+"_realized_pnl.json")
		if err != nil {
			t.Fatal(err)
		}
		pnl, err := snapshot.DecodeRealizedPnL(acct, b)
		if err != nil {
			t.Fatal(err)
		}
		a, _ := bk.Account(acct)
		line, err := Account(a.Ledger, pnl)
		if err != nil {
			t.Fatal(err)
		}
		if !line.OK() {
			t.Errorf("%s: ledger %s − assigned calls %s = %s, Robinhood %s (diff %s)", acct, line.LedgerRealized, line.AssignedCalls, line.Expected, line.Robinhood, line.Difference)
		}
		if line.AssignedCalls.String() != want {
			t.Errorf("%s assigned-call premium = %s, want %s", acct, line.AssignedCalls, want)
		}
	}
}
