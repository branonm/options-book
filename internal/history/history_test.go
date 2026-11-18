package history

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/branonm/options-book/internal/broker/robinhood" // registers the default order sync
)

func ord(id, state, created, updated, processed string) map[string]any {
	return map[string]any{"id": id, "state": state, "created_at": created, "updated_at": updated, "processed_quantity": processed, "chain_symbol": "AAA", "legs": []any{}}
}

// page writes a get_option_orders response to a temp file.
func page(t *testing.T, dir, name string, next string, orders ...map[string]any) string {
	t.Helper()
	data := map[string]any{"orders": orders}
	if next != "" {
		data["next"] = next
	} else {
		data["next"] = nil
	}
	b, _ := json.Marshal(map[string]any{"data": data})
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func mustPull(t *testing.T, files ...string) *Pull {
	t.Helper()
	p, err := ReadPull(files...)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func ts(s string) time.Time { v, _ := time.Parse(time.RFC3339, s); return v }

func TestReadPullPages(t *testing.T) {
	d := t.TempDir()
	p1 := page(t, d, "p1.json", "cursor-2", ord("a", "filled", "2026-11-01T10:00:00Z", "2026-11-01T10:00:00Z", "1"))
	p2 := page(t, d, "p2.json", "", ord("b", "filled", "2026-10-01T10:00:00Z", "2026-10-01T10:00:00Z", "1"))
	p, err := ReadPull(p1, p2)
	if err != nil || len(p.Orders) != 2 {
		t.Fatalf("two pages: %v, %d orders", err, len(p.Orders))
	}
	if _, err := ReadPull(p1); err == nil || !strings.Contains(err.Error(), "fetch the remaining pages") {
		t.Errorf("a cut-off query must be refused: %v", err)
	}
	if _, err := ReadPull(p2, p2); err == nil || !strings.Contains(err.Error(), "no next cursor") {
		t.Errorf("a middle page without a cursor must be refused: %v", err)
	}
}

func TestIncrementalAdd(t *testing.T) {
	d := t.TempDir()
	l, _ := Load(filepath.Join(d, "orders.jsonl"))

	// Full history: a filled open, and an open closer created 10/30.
	full := page(t, d, "full.json", "",
		ord("open1", "filled", "2026-10-30T15:09:00Z", "2026-10-30T15:09:00Z", "3"),
		ord("closer1", "confirmed", "2026-10-30T15:10:00Z", "2026-11-04T13:30:00Z", "0"))
	r, err := l.Add(AddInput{ObservedAt: ts("2026-11-04T17:33:00Z"), FullHistory: true, Pulls: []*Pull{mustPull(t, full)}})
	if err != nil || r.NewOrders != 2 || r.Lines != 2 {
		t.Fatalf("seed: %+v %v", r, err)
	}
	if got := l.OpenIDs(); len(got) != 1 || got[0] != "closer1" {
		t.Fatalf("open ids: %v", got)
	}

	// Incremental pull created since 11/03 that forgets the old closer: refused.
	window := page(t, d, "window.json", "", ord("open2", "filled", "2026-11-06T16:39:00Z", "2026-11-06T16:39:00Z", "3"))
	_, err = l.Add(AddInput{ObservedAt: ts("2026-11-08T01:55:00Z"), CreatedSince: ts("2026-11-03T00:00:00Z"), Pulls: []*Pull{mustPull(t, window)}})
	if err == nil || !strings.Contains(err.Error(), "closer1") {
		t.Fatalf("missing previously-open order must be named: %v", err)
	}

	// A window that starts after the last observation: refused.
	lookup := page(t, d, "lookup.json", "", ord("closer1", "filled", "2026-10-30T15:10:00Z", "2026-11-05T16:15:00Z", "3"))
	_, err = l.Add(AddInput{ObservedAt: ts("2026-11-08T01:55:00Z"), CreatedSince: ts("2026-11-06T00:00:00Z"),
		Pulls: []*Pull{mustPull(t, window), mustPull(t, lookup)}})
	if err == nil || !strings.Contains(err.Error(), "gap") {
		t.Fatalf("window gap must be refused: %v", err)
	}

	// Complete pull: window + single-order lookup of the closer (now filled).
	r, err = l.Add(AddInput{ObservedAt: ts("2026-11-08T01:55:00Z"), CreatedSince: ts("2026-11-03T00:00:00Z"),
		Pulls: []*Pull{mustPull(t, window), mustPull(t, lookup)}})
	if err != nil || r.NewOrders != 1 || r.Changed != 1 || r.Lines != 4 {
		t.Fatalf("incremental: %+v %v", r, err)
	}

	// Replaying the first 2 lines still shows the closer open; all 4 show it filled.
	before, _ := l.OrderRecords(2)
	after, _ := l.OrderRecords(4)
	find := func(id string, n int) string {
		recs, _ := l.OrderRecords(n)
		for _, raw := range recs {
			if h, _ := OrderHead(raw); h.ID == id {
				return h.State
			}
		}
		return "absent"
	}
	if len(before) != 2 || find("closer1", 2) != "confirmed" || find("open2", 2) != "absent" {
		t.Errorf("replay at 2 lines: %d orders, closer %s, open2 %s", len(before), find("closer1", 2), find("open2", 2))
	}
	if len(after) != 3 || find("closer1", 4) != "filled" {
		t.Errorf("replay at 4 lines: %d orders, closer %s", len(after), find("closer1", 4))
	}

	// Re-adding the same pull changes nothing.
	r, err = l.Add(AddInput{ObservedAt: ts("2026-11-09T15:00:00Z"), CreatedSince: ts("2026-11-07T00:00:00Z"),
		Pulls: []*Pull{mustPull(t, window), mustPull(t, lookup)}})
	if err != nil || r.Appended != 0 || r.Unchanged != 2 {
		t.Errorf("idempotent re-add: %+v %v", r, err)
	}

	// A fresh Load reads the same lines, and prefix hashes are stable.
	l2, err := Load(l.Path)
	if err != nil || len(l2.Entries) != 4 {
		t.Fatalf("reload: %v %d", err, len(l2.Entries))
	}
	h1, _ := l.PrefixHash(2)
	h2, _ := l2.PrefixHash(2)
	if h1 != h2 {
		t.Error("prefix hash changed across reload")
	}
}

func TestNextPlan(t *testing.T) {
	d := t.TempDir()
	l, _ := Load(filepath.Join(d, "orders.jsonl"))
	full := page(t, d, "full.json", "", ord("c", "confirmed", "2026-10-30T15:10:00Z", "2026-11-04T13:30:00Z", "0"))
	l.Add(AddInput{ObservedAt: ts("2026-11-08T01:55:00Z"), FullHistory: true, Pulls: []*Pull{mustPull(t, full)}})
	p := l.NextPlan()
	if p.CreatedSince != "2026-11-07" || fmt.Sprint(p.RefreshIDs) != "[c]" {
		t.Errorf("plan: %+v", p)
	}
}

// Robinhood bumps updated_at on resting GTC orders every morning; that alone
// must not add a line.
func TestUpdatedAtTouchIsNotAChange(t *testing.T) {
	d := t.TempDir()
	l, _ := Load(filepath.Join(d, "orders.jsonl"))
	a := page(t, d, "a.json", "", ord("c", "confirmed", "2026-10-20T14:42:00Z", "2026-11-04T13:30:00Z", "0"))
	l.Add(AddInput{ObservedAt: ts("2026-11-04T17:33:00Z"), FullHistory: true, Pulls: []*Pull{mustPull(t, a)}})
	b := page(t, d, "b.json", "", ord("c", "confirmed", "2026-10-20T14:42:00Z", "2026-11-06T13:30:00Z", "0"))
	r, err := l.Add(AddInput{ObservedAt: ts("2026-11-08T01:55:00Z"), CreatedSince: ts("2026-11-03T00:00:00Z"), Pulls: []*Pull{mustPull(t, b)}})
	if err != nil || r.Appended != 0 || r.Unchanged != 1 {
		t.Errorf("daily touch appended a line: %+v %v", r, err)
	}
}
