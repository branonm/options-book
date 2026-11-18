package diff

import (
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/branonm/options-book/internal/config"
	"github.com/branonm/options-book/internal/layout"
	"github.com/branonm/options-book/internal/rules"
	"github.com/branonm/options-book/internal/store/storetest"
)

// 2026-02-06 → 2026-02-13 in the synthetic repo (testdata/synthetic/README.md):
// the off-rule $1.10 BBB closer was cancelled and replaced by the Rule 10
// $1.00, which filled on 2/10 for a $100 net.
func TestDiff0206To0213(t *testing.T) {
	root := storetest.Root(t)
	cfg, err := config.Load(root, layout.DefaultBook)
	if err != nil {
		t.Fatal(err)
	}
	r, err := rules.Load(filepath.Join(root, filepath.FromSlash(layout.RulesFile(layout.DefaultBook))))
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range storetest.Sources(t) {
		t.Run(src.Name, func(t *testing.T) { check0206To0213(t, src.FS, cfg, r) })
	}
}

func check0206To0213(t *testing.T, fsys fs.FS, cfg *config.Book, r *rules.Rules) {
	rep := Compare(side(t, fsys, cfg, "2026-02-06"), side(t, fsys, cfg, "2026-02-13"), r)

	if v := rep.Realized["roth"]; !v[0].Equal(d("150")) || !v[1].Equal(d("250")) {
		t.Errorf("roth realized %s → %s, want 150 → 250", v[0], v[1])
	}
	if v := rep.Realized["trad"]; !v[0].Equal(d("100")) || !v[1].Equal(d("100")) {
		t.Errorf("trad realized %v, want 100 → 100", v)
	}
	if len(rep.Closed) != 1 || rep.Closed[0].Leg != "BBB $85c 2026-02-20" || !rep.Closed[0].Net.Equal(d("100")) || rep.Closed[0].Date != "2026-02-10" {
		t.Errorf("closed legs: %+v", rep.Closed)
	}
	if len(rep.Opened) != 0 {
		t.Errorf("opened legs: %+v", rep.Opened)
	}
	var transitions []string
	for _, o := range rep.Orders {
		transitions = append(transitions, o.Leg+" "+o.Action+" "+o.From+">"+o.To+" "+o.Tif)
	}
	got := strings.Join(transitions, "\n")
	for _, want := range []string{
		"BBB $85c 2026-02-20 buy to close confirmed>cancelled gtc",
		"BBB $85c 2026-02-20 buy to close >filled gtc",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing order change %q in:\n%s", want, got)
		}
	}
	if len(rep.Orders) != 2 {
		t.Errorf("%d order changes, want 2:\n%s", len(rep.Orders), got)
	}
	if len(rep.Blocks) != 1 || rep.Blocks[0].Symbol != "BBB" || !rep.Blocks[0].BreakevenTo.Round(2).Equal(d("79.00")) {
		t.Errorf("blocks: %+v", rep.Blocks)
	}
	res := map[string]bool{}
	for _, f := range rep.Resolved {
		res[f.Subject+" "+f.Expected] = true
	}
	if !res["BBB $85c 2026-02-20 $1.00"] {
		t.Errorf("want the BBB closer finding resolved; got %v", res)
	}
	if len(rep.New) != 1 || rep.New[0].Subject != "BBB 100 sh" {
		t.Errorf("new findings: %+v, want the BBB uncovered-shares alert", rep.New)
	}
}

func TestDiffSameSnapshotIsEmpty(t *testing.T) {
	root := storetest.Root(t)
	cfg, _ := config.Load(root, layout.DefaultBook)
	r, _ := rules.Load(filepath.Join(root, filepath.FromSlash(layout.RulesFile(layout.DefaultBook))))
	s := side(t, storetest.Sources(t)[0].FS, cfg, "2026-02-13")
	rep := Compare(s, s, r)
	var b strings.Builder
	rep.Markdown(&b)
	if !strings.Contains(b.String(), "Nothing changed.") {
		t.Errorf("same snapshot should report nothing:\n%s", b.String())
	}
}
