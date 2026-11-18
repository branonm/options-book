package wheel

import (
	"encoding/json"
	"github.com/branonm/options-book/internal/layout"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/branonm/options-book/internal/config"
	"github.com/branonm/options-book/internal/rules"
	"github.com/branonm/options-book/internal/rules/rulestest"
	"github.com/branonm/options-book/internal/snapshot"
	"github.com/branonm/options-book/internal/store/storetest"
)

type wantFinding struct {
	Account  string `json:"account"`
	Check    string `json:"check"`
	Subject  string `json:"subject"`
	Expected string `json:"expected,omitempty"`
	Actual   string `json:"actual,omitempty"`
}

// TestCheckinSnapshots runs the full check-in over every snapshot whose
// expected.json has a "checkin" section and requires the defects and alerts
// to match exactly: nothing missing, nothing extra.
func TestCheckinSnapshots(t *testing.T) {
	root := storetest.Root(t)
	cfg, err := config.Load(root, layout.DefaultBook)
	if err != nil {
		t.Fatal(err)
	}
	r, err := rules.Load(filepath.Join(root, filepath.FromSlash(layout.RulesFile(layout.DefaultBook))))
	if err != nil {
		t.Fatal(err)
	}
	rulestest.ForSnapshots(r)
	for _, src := range storetest.Sources(t) {
		t.Run(src.Name, func(t *testing.T) { checkinSnapshots(t, src.FS, cfg, r) })
	}
}

func checkinSnapshots(t *testing.T, fsys fs.FS, cfg *config.Book, r *rules.Rules) {
	matches, _ := fs.Glob(fsys, "snapshots/*/expected.json")
	ran := 0
	for _, m := range matches {
		var want struct {
			Checkin *struct {
				Defects []wantFinding `json:"defects"`
				Alerts  []wantFinding `json:"alerts"`
			} `json:"checkin"`
		}
		b, err := fs.ReadFile(fsys, m)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &want); err != nil {
			t.Fatal(err)
		}
		if want.Checkin == nil {
			continue
		}
		ran++
		dir := path.Dir(m)
		t.Run(path.Base(dir), func(t *testing.T) {
			asOf, err := time.Parse("2006-01-02", path.Base(dir))
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, a := range cfg.Accounts {
				names = append(names, a.Name)
			}
			snap, err := snapshot.LoadAt(snapshot.At{FS: fsys, Dir: dir}, names)
			if err != nil {
				t.Fatal(err)
			}
			bk, err := Assemble(cfg, snap, asOf)
			if err != nil {
				t.Fatal(err)
			}
			rep := bk.Check(r)
			compare(t, "defect", want.Checkin.Defects, rep.Defects, true)
			compare(t, "alert", want.Checkin.Alerts, rep.Alerts, false)
		})
	}
	if ran == 0 {
		t.Fatal("no snapshot has a checkin expectation")
	}
}

func compare(t *testing.T, kind string, want []wantFinding, got []Finding, values bool) {
	t.Helper()
	key := func(a, c, s, e, x string) string {
		if !values {
			e, x = "", ""
		}
		return a + "|" + c + "|" + s + "|" + e + "|" + x
	}
	w := map[string]bool{}
	for _, f := range want {
		w[key(f.Account, f.Check, f.Subject, f.Expected, f.Actual)] = true
	}
	g := map[string]bool{}
	var extra []string
	for _, f := range got {
		k := key(f.Account, f.Check, f.Subject, f.Expected, f.Actual)
		g[k] = true
		if !w[k] {
			extra = append(extra, f.String())
		}
	}
	var missing []string
	for k := range w {
		if !g[k] {
			missing = append(missing, k)
		}
	}
	sort.Strings(extra)
	sort.Strings(missing)
	for _, s := range extra {
		t.Errorf("unexpected %s: %s", kind, s)
	}
	for _, s := range missing {
		t.Errorf("missing %s: %s", kind, s)
	}
}
