package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/branonm/options-book/internal/layout"
)

// Item 6 step 3b's gate: every command that reads a book gives the same output
// and exit code with and without --book ira-wheel (the default book), on every
// snapshot, as text and as JSON.
func TestBookFlagIsIdentical(t *testing.T) {
	ids := snapshotIDs(t)
	n := 0
	same := func(label string, f func([]string) (int, error), args ...string) {
		t.Helper()
		a := cli(t, f, args...)
		b := cli(t, f, append(append([]string{}, args...), "--book", layout.DefaultBook)...)
		n++
		if a.err != nil || b.err != nil || a.text != b.text || a.code != b.code {
			t.Errorf("%s: output differs with --book %s (errors %v / %v)", label, layout.DefaultBook, a.err, b.err)
		}
		if a.text == "" {
			t.Errorf("%s: empty output", label)
		}
	}
	for _, id := range ids {
		dir := "snapshots/" + id
		for _, js := range []string{"", "--json"} {
			args := []string{"--snapshot", dir}
			if js != "" {
				args = append(args, js)
			}
			same("checkin "+id+js, runCheckin, args...)
			same("ledger "+id+js, noCode(runLedger), args...)
		}
		same("book "+id, noCode(runBook), "--snapshot", dir)
	}
	for i := 1; i < len(ids); i++ {
		if a := cli(t, noCode(runDiff), "--from", "snapshots/"+ids[i-1], "--to", "snapshots/"+ids[i]); a.err == nil {
			same("diff "+ids[i], noCode(runDiff), "--from", "snapshots/"+ids[i-1], "--to", "snapshots/"+ids[i])
		}
	}
	same("reconcile", runReconcile, "--snapshot", "snapshots/2026-02-13",
		"--pnl", "roth="+layout.PnL("2026-02-13", "roth"), "--pnl", "trad="+layout.PnL("2026-02-13", "trad"))
	same("ticket", runTicket, "--snapshot", "snapshots/2026-02-13", "--account", "roth", "--kind", "covered_call",
		"--option", "a0000000-0000-4000-8000-000000000021", "--limit", "1.25")
	t.Logf("%d commands identical with and without --book %s", n, layout.DefaultBook)
}

// --config, --accounts and --rules name files explicitly, and win over --book.
func TestBookFlagOverrides(t *testing.T) {
	base := cli(t, runCheckin, "--snapshot", "snapshots/2026-02-13")
	if base.err != nil {
		t.Fatal(base.err)
	}
	same := cli(t, runCheckin, "--snapshot", "snapshots/2026-02-13",
		"--config", filepath.FromSlash(layout.BookFile(layout.DefaultBook)),
		"--accounts", filepath.FromSlash(layout.AccountsConfig),
		"--rules", filepath.FromSlash(layout.RulesFile(layout.DefaultBook)))
	if same.err != nil || same.text != base.text || same.code != base.code {
		t.Errorf("explicit files differ from the book's own: %v", same.err)
	}
	// A rules file that really is different shows up: an override is an override.
	rules, _ := os.ReadFile(layout.RulesFile(layout.DefaultBook))
	// The live rules carry no concentration cap; a file that adds a tight one does.
	capped := strings.Replace(string(rules), `"lot_size"`, `"concentration_caps": [{"name": "metals", "rule": 4, "symbols": ["AAA", "BBB"], "max_share_of_invested_equity": "0.01"}], "lot_size"`, 1)
	if capped == string(rules) {
		t.Fatal("rules.json has no lot_size entry to anchor the test cap; update the test")
	}
	tight := filepath.Join(t.TempDir(), "rules.json")
	if err := os.WriteFile(tight, []byte(capped), 0o644); err != nil {
		t.Fatal(err)
	}
	if o := cli(t, runCheckin, "--snapshot", "snapshots/2026-02-13", "--rules", tight); o.err != nil || o.text == base.text {
		t.Errorf("--rules didn't override the book's rules: %v", o.err)
	}
}

// An unknown book is refused with the file it looked for, not run on defaults.
func TestUnknownBookIsRefused(t *testing.T) {
	for name, f := range map[string]func([]string) (int, error){"checkin": runCheckin, "ledger": noCode(runLedger), "book": noCode(runBook)} {
		o := cli(t, f, "--snapshot", "snapshots/2026-02-13", "--book", "nope")
		if o.err == nil || !strings.Contains(o.err.Error(), "no book \"nope\"") || !strings.Contains(o.err.Error(), layout.BookDir("nope")) {
			t.Errorf("%s --book nope: %v", name, o.err)
		}
	}
}

// --write splices into the book's own doc (book.json's doc), --into stays as it was.
func TestBookWrite(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	for _, d := range []string{"books", "config", "snapshots", "history"} {
		copyTree(t, filepath.Join(repoDir, d), filepath.Join(root, d))
	}
	doc := "# Wheel book\n\nProse above.\n\n<!-- BEGIN GENERATED: wheel book -->\nstale\n<!-- END GENERATED -->\n\nProse below.\n"
	if err := os.WriteFile("wheel-book.md", []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	if o := cli(t, noCode(runBook), "--snapshot", "snapshots/2026-02-13", "--write", "--into", "x.md"); o.err == nil {
		t.Error("--write with --into was accepted")
	}
	if o := cli(t, noCode(runBook), "--snapshot", "snapshots/2026-02-13", "--write"); o.err != nil {
		t.Fatal(o.err)
	}
	got, _ := os.ReadFile("wheel-book.md")
	if !strings.Contains(string(got), "Prose above.") || !strings.Contains(string(got), "Prose below.") || strings.Contains(string(got), "stale") ||
		!strings.Contains(string(got), "from `snapshots/2026-02-13`") {
		t.Errorf("wheel-book.md after --write:\n%s", got)
	}
}
