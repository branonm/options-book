package history

import (
	"path/filepath"
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
