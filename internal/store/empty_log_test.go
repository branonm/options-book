package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/branonm/options-book/internal/history"
)

// B9: an account with no orders has a position in its order log: zero lines at
// the chain's seed. history_add records it instead of refusing, the store reads
// it back (Open re-checks it), the log's last observation moves with each
// quiet pull, and the first order appends as line 1.
func TestEmptyLogHasPositionZero(t *testing.T) {
	t.Run("memory", func(t *testing.T) { emptyLogPosition(t, NewMem()) })
	t.Run("postgres", func(t *testing.T) { emptyLogPosition(t, scratchDB(t)) })
}

func emptyLogPosition(t *testing.T, db DB) {
	ctx := context.Background()
	t0 := time.Date(2026, 11, 18, 15, 0, 0, 0, time.UTC)
	const acct = "paper"
	pull := func(snap, page, body string, at time.Time, full bool) {
		t.Helper()
		if err := OpenSnapshot(ctx, db, snap, "", at); err != nil {
			t.Fatal(err)
		}
		if _, err := IngestResponse(ctx, db, Ingest{Snapshot: snap, Role: RoleOrdersPull, Account: acct, Instrument: page,
			Body: []byte(body), Verbatim: true, Source: "alpaca/paper-rest", Broker: "alpaca"}, at); err != nil {
			t.Fatal(err)
		}
		src, err := Open(ctx, db)
		if err != nil {
			t.Fatal(err)
		}
		in := HistoryAdd{Snapshot: snap, Account: acct, ObservedAt: at, Pulls: [][]string{{page}}, Broker: "alpaca"}
		if full {
			in.Full = true
		} else {
			in.CreatedSince = at.AddDate(0, 0, -1)
		}
		if _, err := AddHistory(ctx, db, src, in); err != nil {
			t.Fatalf("history_add on %s: %v", snap, err)
		}
	}
	position := func(snap string) Doc {
		t.Helper()
		d, err := db.Get(ctx, COrderPos, snap+"|"+acct)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	observed := func() string {
		t.Helper()
		src, err := Open(ctx, db)
		if err != nil {
			t.Fatalf("a stored empty position must read back: %v", err)
		}
		l, err := history.LoadFSFor(src.FS, LogPath(acct), "alpaca")
		if err != nil {
			t.Fatal(err)
		}
		return l.LastObserved()
	}

	pull("2026-11-18", "full_p1", `[]`, t0, true)
	d := position("2026-11-18")
	if d["seq"] != int64(0) || d["chain_hash"] != "" || d["prefix_sha256"] != sum(nil) {
		t.Errorf("empty position = %v, want seq 0, the chain's seed \"\", and the hash of no lines", d)
	}
	if got := observed(); got != "2026-11-18T15:00:00Z" {
		t.Errorf("last observed after the empty pull = %q", got)
	}

	// A second quiet pull is incremental, and a stale one is refused.
	pull("2026-11-19", "window_p1", `[]`, t0.Add(24*time.Hour), false)
	if got := observed(); got != "2026-11-19T15:00:00Z" {
		t.Errorf("last observed after a second quiet pull = %q", got)
	}
	if err := OpenSnapshot(ctx, db, "2026-11-20", "", t0); err != nil {
		t.Fatal(err)
	}
	if _, err := IngestResponse(ctx, db, Ingest{Snapshot: "2026-11-20", Role: RoleOrdersPull, Account: acct, Instrument: "window_p1",
		Body: []byte(`[]`), Verbatim: true, Source: "alpaca/paper-rest", Broker: "alpaca"}, t0); err != nil {
		t.Fatal(err)
	}
	src, _ := Open(ctx, db)
	_, err := AddHistory(ctx, db, src, HistoryAdd{Snapshot: "2026-11-20", Account: acct, ObservedAt: t0, CreatedSince: t0.AddDate(0, 0, -1),
		Pulls: [][]string{{"window_p1"}}, Broker: "alpaca"})
	if err == nil || !strings.Contains(err.Error(), "not after the log's last observation 2026-11-19T15:00:00Z") {
		t.Errorf("a pull observed before the last quiet pull: err = %v", err)
	}

	// The empty positions survive export → import, and the rebuilt store reads the same files.
	out := map[string][]byte{}
	if err := Export(ctx, db, mapSink(out)); err != nil {
		t.Fatal(err)
	}
	rebuilt := NewMem()
	if err := ImportExport(ctx, rebuilt, exportFS(out)); err != nil {
		t.Fatalf("import of an export holding empty positions: %v", err)
	}
	if _, err := Verify(ctx, rebuilt); err != nil {
		t.Errorf("verify after export → import: %v", err)
	}
}
