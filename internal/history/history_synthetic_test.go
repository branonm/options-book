package history

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The synthetic record: seed from the 2026-02-06 full pull, then add 2026-02-13
// as an incremental update (the window page, the open page and one by-id lookup).
// Replays must reproduce each snapshot's order list: at 2/06 the off-rule BBB
// closer (order 6) is confirmed; at 2/13 it is cancelled and its replacement
// (order 7) filled.
func TestSyntheticHistory0206To0213(t *testing.T) {
	root := filepath.Join("..", "..", "testdata", "synthetic", "snapshots")
	const bad, good = "b0000006-0000-4000-8000-000000000006", "b0000007-0000-4000-8000-000000000007"
	l, _ := Load(filepath.Join(t.TempDir(), "orders.jsonl"))
	p06 := mustPull(t, filepath.Join(root, "2026-02-06", "roth", "orders_pull", "full_p1.json"))
	r, err := l.Add(AddInput{ObservedAt: ts("2026-02-06T22:00:00Z"), FullHistory: true, Pulls: []*Pull{p06}})
	if err != nil {
		t.Fatal(err)
	}
	n06 := r.Lines
	pulls := []*Pull{
		mustPull(t, filepath.Join(root, "2026-02-13", "roth", "orders_pull", "window_p1.json")),
		mustPull(t, filepath.Join(root, "2026-02-13", "roth", "orders_pull", "open_p1.json")),
		mustPull(t, filepath.Join(root, "2026-02-13", "roth", "orders_pull", "lookup_b0000006.json")),
	}
	if _, err := l.Add(AddInput{ObservedAt: ts("2026-02-13T22:00:00Z"), CreatedSince: ts("2026-02-06T00:00:00Z"), Pulls: pulls}); err != nil {
		t.Fatal(err)
	}
	states := func(n int) map[string]string {
		got, err := l.OrderRecords(n)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]string{}
		for _, raw := range got {
			h, _ := OrderHead(raw)
			m[h.ID] = h.State
		}
		return m
	}
	a := states(n06)
	if len(a) != 6 || a[bad] != "confirmed" || a[good] != "" {
		t.Errorf("replay at 2/06: %d orders, closer %q, replacement %q; want 6, confirmed, absent", len(a), a[bad], a[good])
	}
	b := states(len(l.Entries))
	if len(b) != 7 || b[bad] != "cancelled" || b[good] != "filled" {
		t.Errorf("replay at 2/13: %d orders, closer %q, replacement %q; want 7, cancelled, filled", len(b), b[bad], b[good])
	}
}

// B12: a pull that finds nothing new appends no line, but the snapshot's
// position records it, and the next plan must start from it. Two quiet pulls
// in a row move the window forward each time.
func TestQuietPullsAdvanceTheWindow(t *testing.T) {
	root := filepath.Join("..", "..", "testdata", "synthetic", "snapshots")
	repo := t.TempDir()
	logPath := filepath.Join(repo, "history", "roth", "orders.jsonl")
	l, _ := Load(logPath)
	full := mustPull(t, filepath.Join(root, "2026-02-06", "roth", "orders_pull", "full_p1.json"))
	if _, err := l.Add(AddInput{ObservedAt: ts("2026-02-06T22:00:00Z"), FullHistory: true, Pulls: []*Pull{full}}); err != nil {
		t.Fatal(err)
	}
	record := func(snap, observed string) { // what history add leaves in the snapshot
		dir := filepath.Join(repo, "snapshots", snap, "roth")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		ref, _ := json.Marshal(Ref{Log: "history/roth/orders.jsonl", Lines: len(l.Entries), ObservedAt: observed})
		if err := os.WriteFile(filepath.Join(dir, "history.json"), ref, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	reload := func() *Log {
		l, err := Load(logPath)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	if got := reload().NextPlan().CreatedSince; got != "2026-02-05" {
		t.Fatalf("window after the full pull = %s, want 2026-02-05", got)
	}

	// A quiet pull on 2/13 appends nothing; its position is the only record.
	record("2026-02-13", "2026-02-13T22:00:00Z")
	l = reload()
	if got := l.LastObserved(); got != "2026-02-13T22:00:00Z" {
		t.Errorf("last observed = %s, want the 2/13 pull", got)
	}
	if got := l.NextPlan().CreatedSince; got != "2026-02-12" {
		t.Errorf("window after one quiet pull = %s, want 2026-02-12", got)
	}

	// The next add is checked against that observation, not the log's last line.
	open := mustPull(t, filepath.Join(root, "2026-02-13", "roth", "orders_pull", "open_p1.json"))
	stale := AddInput{ObservedAt: ts("2026-02-13T20:00:00Z"), CreatedSince: ts("2026-02-12T00:00:00Z"), Pulls: []*Pull{open}}
	if _, _, err := l.Prepare(stale); err == nil || !strings.Contains(err.Error(), "not after the log's last observation 2026-02-13T22:00:00Z") {
		t.Errorf("an observation before the quiet pull: err = %v, want a refusal naming 2026-02-13T22:00:00Z", err)
	}

	record("2026-02-20", "2026-02-20T22:00:00Z")
	if got := reload().NextPlan().CreatedSince; got != "2026-02-19" {
		t.Errorf("window after two quiet pulls = %s, want 2026-02-19", got)
	}
}
