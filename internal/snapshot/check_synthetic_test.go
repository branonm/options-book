package snapshot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var refs = []AccountRef{{"roth", "Roth IRA ••0001"}, {"trad", "Traditional IRA ••0002"}}

// synthRepo is the synthetic repo at testdata/synthetic.
var synthRepo = filepath.Join("..", "..", "testdata", "synthetic")

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// copyRepo copies a synthetic snapshot (without its manifest) and the order
// log it replays into a temp repo, and returns the repo and the snapshot dir.
// The snapshots here carry history.json rather than a full order dump, so the
// log has to come with them.
func copyRepo(t *testing.T, name string) (repo, snap string) {
	t.Helper()
	repo = t.TempDir()
	snap = filepath.Join(repo, "snapshots", name)
	src := filepath.Join(synthRepo, "snapshots", name)
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || filepath.Base(p) == manifestName {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		copyFile(t, p, filepath.Join(snap, rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, acct := range []string{"roth", "trad"} {
		rel := filepath.Join("history", acct, "orders.jsonl")
		copyFile(t, filepath.Join(synthRepo, rel), filepath.Join(repo, rel))
	}
	return repo, snap
}

func problems(ps []Problem) string {
	var s []string
	for _, p := range ps {
		s = append(s, p.File+": "+p.Problem)
	}
	return strings.Join(s, "\n")
}

func TestCheckCompleteSnapshot(t *testing.T) {
	_, dir := copyRepo(t, "2026-02-13")
	if ps := Check(dir, refs); len(ps) != 0 {
		t.Fatalf("the 2/13 snapshot should be complete:\n%s", problems(ps))
	}
}

func TestCheckFindsGapsAndNamesTheFix(t *testing.T) {
	_, dir := copyRepo(t, "2026-02-13")
	os.Remove(filepath.Join(dir, "trad", "portfolio.json"))
	os.Remove(filepath.Join(dir, "option_quotes.json"))
	ps := Check(dir, refs)
	got := problems(ps)
	for _, want := range []string{"trad/portfolio.json: missing", "option_quotes.json: missing"} {
		if !strings.Contains(got, want) {
			t.Errorf("want %q in:\n%s", want, got)
		}
	}
	for _, p := range ps {
		// 2/13's open contracts: roth's AAA $55c 3/20 and trad's CCC $44c 2/20.
		if p.File == "option_quotes.json" && !(strings.Contains(p.Fix, "a0000000-0000-4000-8000-000000000002") && strings.Contains(p.Fix, "a0000000-0000-4000-8000-000000000012")) {
			t.Errorf("fix should list the open contracts' ids: %s", p.Fix)
		}
		if p.File == "trad/portfolio.json" && !strings.Contains(p.Fix, "get_portfolio(Traditional IRA ••0002)") {
			t.Errorf("fix should name the call: %s", p.Fix)
		}
	}
}

func TestManifestDetectsEdits(t *testing.T) {
	_, dir := copyRepo(t, "2026-02-13")
	if _, err := WriteManifest(dir, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteManifest(dir, time.Now()); err == nil {
		t.Error("second manifest write must be refused")
	}
	if ps := Check(dir, refs); len(ps) != 0 {
		t.Fatalf("fresh manifest should verify:\n%s", problems(ps))
	}
	p := filepath.Join(dir, "roth", "equity_positions.json")
	b, _ := os.ReadFile(p)
	os.WriteFile(p, []byte(strings.Replace(string(b), "50.000000", "50.010000", 1)), 0o644)
	os.WriteFile(filepath.Join(dir, "extra.json"), []byte("{}"), 0o644)
	got := problems(Check(dir, refs))
	for _, want := range []string{"roth/equity_positions.json: changed since the manifest", "extra.json: added after the manifest"} {
		if !strings.Contains(got, want) {
			t.Errorf("want %q in:\n%s", want, got)
		}
	}
}

// A snapshot that replays the history log must load the orders the log holds
// at its position (roth 7, trad 3 at 2/13), and a rewritten log must be caught.
func TestHistoryRefSnapshot(t *testing.T) {
	repo, snap := copyRepo(t, "2026-02-13")
	got, err := Load(snap, []string{"roth", "trad"})
	if err != nil {
		t.Fatal(err)
	}
	for acct, want := range map[string]int{"roth": 7, "trad": 3} {
		if n := len(got.Accounts[acct].Orders); n != want || !got.Accounts[acct].OrdersAllStates {
			t.Errorf("%s: %d orders via history, want %d (all states)", acct, n, want)
		}
	}
	states := map[string]string{}
	for _, o := range got.Accounts["roth"].Orders {
		states[o.ID] = o.BrokerState
	}
	if states["b0000006-0000-4000-8000-000000000006"] != "cancelled" || states["b0000007-0000-4000-8000-000000000007"] != "filled" {
		t.Errorf("roth's replaced closer: %v", states)
	}
	if ps := Check(snap, refs); len(ps) != 0 {
		t.Fatalf("history-backed snapshot should check clean:\n%s", problems(ps))
	}

	// Rewrite one early line of the Roth log: the snapshot must refuse to load.
	logPath := filepath.Join(repo, "history", "roth", "orders.jsonl")
	b, _ := os.ReadFile(logPath)
	os.WriteFile(logPath, []byte(strings.Replace(string(b), `"state":"filled"`, `"state":"cancelled"`, 1)), 0o644)
	if _, err := Load(snap, []string{"roth", "trad"}); err == nil || !strings.Contains(err.Error(), "rewritten") {
		t.Errorf("rewritten log must be refused: %v", err)
	}
	if got := problems(Check(snap, refs)); !strings.Contains(got, "roth/history.json") {
		t.Errorf("check should flag the rewritten log:\n%s", got)
	}
}
