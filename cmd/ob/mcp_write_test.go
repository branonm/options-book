package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/branonm/options-book/internal/layout"
	"io/fs"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/branonm/options-book/internal/history"
	"github.com/branonm/options-book/internal/obmcp"
	"github.com/branonm/options-book/internal/snapshot"
	"github.com/branonm/options-book/internal/store"
)

// replayed are the check-ins the gate redoes through the server, in order.
// Each one's history.json, manifest.json and log lines must come out of the
// write tools byte for byte as the file-based run wrote them. The synthetic
// repo has one after its first (2026-02-06): 2026-02-13.
var replayed = []string{"2026-02-13"}

// notReplayed are left out of the before state and of the comparison. The real
// repo had one (an intraday snapshot made by copying files, not by the
// procedure); the synthetic repo has none.
var notReplayed []string

// before is the repo as it stood when 2026-02-06 was the latest
// snapshot, without the snapshots the gate replays.
func before(t *testing.T) fs.FS {
	return repoAt(t, "2026-02-06", append(append([]string{}, replayed...), notReplayed...))
}

// repoAt is the repo as it stood when snapshot last was the latest: no
// snapshot from a later session, each order log cut at last's position, and
// the snapshots in drop left out. Pinned this way, the gate never depends on
// check-ins made after it was written (B4).
func repoAt(t *testing.T, last string, drop []string) fs.FS {
	t.Helper()
	disk := os.DirFS(".")
	lastID, err := snapshot.ParseID(last)
	if err != nil {
		t.Fatal(err)
	}
	cut := map[string]int{}
	for _, acct := range []string{"roth", "trad"} {
		b, err := fs.ReadFile(disk, "snapshots/"+last+"/"+acct+"/history.json")
		if err != nil {
			t.Fatal(err)
		}
		var ref history.Ref
		json.Unmarshal(b, &ref)
		cut[ref.Log] = ref.Lines
	}
	skip := map[string]bool{}
	for _, id := range drop {
		skip["snapshots/"+id] = true
		skip["reconcile/"+id] = true // a reconcile folder is named for its snapshot
	}
	later := func(p string) bool {
		dir, name := path.Split(p)
		if dir != "snapshots/" && dir != "reconcile/" {
			return false
		}
		id, err := snapshot.ParseID(name)
		return err == nil && id.Date.After(lastID.Date)
	}
	out := fstest.MapFS{}
	for _, top := range []string{"snapshots", "history", "reconcile"} {
		err := fs.WalkDir(disk, top, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if skip[p] || later(p) {
					return fs.SkipDir
				}
				return nil
			}
			b, err := fs.ReadFile(disk, p)
			if err != nil {
				return err
			}
			if n, ok := cut[p]; ok {
				lines := bytes.SplitAfter(b, []byte("\n"))
				b = bytes.Join(lines[:n], nil)
			}
			out[p] = &fstest.MapFile{Data: b}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// writeSession serves every tool over MCP against a store holding fsys,
// with a clock the test sets.
func writeSession(t *testing.T, fsys fs.FS) (*mcp.ClientSession, store.DB, *time.Time) {
	t.Helper()
	ctx := context.Background()
	var db store.DB = store.NewMem()
	if dsn := os.Getenv("POSTGRES_DSN"); dsn != "" {
		// A scratch schema per session: the real create semantics, append-only trigger included.
		p, cleanup, err := store.ScratchPostgres(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(cleanup)
		db = p
	}
	if _, err := store.ImportRepo(ctx, db, fsys); err != nil {
		t.Fatal(err)
	}
	bks, accts, asha := testBooks(t)
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC) // far from any snapshot: nothing may date a figure by it (B3)
	server := mcp.NewServer(&mcp.Implementation{Name: "options-book", Version: "test"}, nil)
	obmcp.Register(server, &obmcp.Deps{
		Books: bks, DefaultBook: layout.DefaultBook, Accounts: accts, AccountsSHA: asha, DB: db, Spec: "memory", Commit: "test", CacheTTL: time.Hour,
		Now: func() time.Time { return now },
	})
	ct, st := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs, db, &now
}

func mustTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) toolOut {
	t.Helper()
	out := callTool(t, cs, name, args)
	if out.isError {
		t.Fatalf("%s %v: %s", name, args, out.text)
	}
	return out
}

var createdSince = regexp.MustCompile(`created_at_gte=(\d{4}-\d{2}-\d{2})`)

// checkinThroughServer is one check-in done through the write tools, from
// the snapshot's files on disk, the way a session would do it.
func checkinThroughServer(t *testing.T, cs *mcp.ClientSession, now *time.Time, id string) {
	t.Helper()
	disk := os.DirFS("snapshots/" + id)
	mustTool(t, cs, "open_snapshot", map[string]any{"snapshot": id, "note": "replayed by mcp_write_test"})

	var files []string
	fs.WalkDir(disk, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, p)
		}
		return err
	})
	sort.Strings(files)
	pages := map[string][]string{}
	for _, rel := range files {
		m, ok, err := store.MemberFor(id, rel)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			continue // history.json and manifest.json come from history_add and seal_snapshot
		}
		body, _ := fs.ReadFile(disk, rel)
		args := map[string]any{"snapshot": id, "role": m.Role, "body": string(body), "verbatim": false,
			"note": "replayed from the repo file by mcp_write_test"}
		if m.Account != "" {
			args["account"] = m.Account
		}
		switch m.Role {
		case store.RoleOrdersPull:
			args["page"] = m.Instrument
			pages[m.Account] = append(pages[m.Account], m.Instrument)
		case store.RoleOptionQuote, store.RoleOptionInstrument:
			args["instrument"] = m.Instrument
		}
		if src, call := store.CallFor(m.Role); src == "robinhood" {
			args["call"] = call
		}
		mustTool(t, cs, "ingest", args)
	}

	for _, acct := range []string{"roth", "trad"} {
		b, err := fs.ReadFile(disk, acct+"/history.json")
		if err != nil {
			t.Fatal(err)
		}
		var ref history.Ref
		json.Unmarshal(b, &ref)
		plan := mustTool(t, cs, "history_plan", map[string]any{"account": acct})
		since := createdSince.FindStringSubmatch(plan.text)
		if since == nil {
			t.Fatalf("history_plan %s: no window in %q", acct, plan.text)
		}
		var pulls [][]string
		for _, pg := range pages[acct] {
			pulls = append(pulls, []string{pg}) // every saved query is one page
		}
		mustTool(t, cs, "history_add", map[string]any{"snapshot": id, "account": acct,
			"observed_at": ref.ObservedAt, "created_since": since[1], "pulls": pulls})
	}

	m, err := snapshot.ReadManifest("snapshots/" + id)
	if err != nil {
		t.Fatal(err)
	}
	*now, _ = time.Parse(time.RFC3339, m.CreatedAt) // the file run's seal time, so manifest.json can match
	out := mustTool(t, cs, "seal_snapshot", map[string]any{"snapshot": id})
	if out.exitCode != 0 || !strings.Contains(out.text, "is now read-only") {
		t.Fatalf("seal %s: %d %s", id, out.exitCode, out.text)
	}
	*now = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)

	// What may follow a seal: the snapshot's saved realized P&L, if it has any.
	for _, acct := range []string{"roth", "trad"} {
		pnl, err := os.ReadFile(layout.PnL(id, acct))
		if err != nil {
			continue
		}
		mustTool(t, cs, "ingest", map[string]any{"snapshot": id, "role": "realized_pnl", "account": acct,
			"call": "get_realized_pnl", "body": string(pnl), "verbatim": false, "note": "replayed from the repo file by mcp_write_test"})
	}
}

// TestCheckinThroughServer is phase 4's gate: the synthetic check-in
// of 2/13, done through the write tools, reproduce the file-based runs —
// every input, history.json, manifest.json and order-log line byte for byte,
// so every figure — and every read tool still matches the CLI.
func TestCheckinThroughServer(t *testing.T) {
	ctx := context.Background()
	cs, db, now := writeSession(t, before(t))
	for _, id := range replayed {
		checkinThroughServer(t, cs, now, id)
	}

	// The rebuilt store reads back as today's repo does.
	got, err := store.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	full := store.NewMem()
	if _, err := store.ImportRepo(ctx, full, repoAt(t, "2026-02-13", nil)); err != nil {
		t.Fatal(err)
	}
	want, err := store.Open(ctx, full)
	if err != nil {
		t.Fatal(err)
	}
	gotFiles, wantFiles := filesOf(t, got.FS), filesOf(t, want.FS)
	for p := range wantFiles {
		for _, id := range notReplayed {
			if strings.HasPrefix(p, "snapshots/"+id+"/") {
				delete(wantFiles, p)
			}
		}
	}
	for p, b := range wantFiles {
		if g, ok := gotFiles[p]; !ok {
			t.Errorf("%s: missing after the replay", p)
		} else if !bytes.Equal(g, b) {
			t.Errorf("%s: differs from the file-based run", p)
		}
	}
	for p := range gotFiles {
		if _, ok := wantFiles[p]; !ok {
			t.Errorf("%s: produced by the replay but not by the file run", p)
		}
	}
	t.Logf("replayed %d check-ins; %d files compared", len(replayed), len(wantFiles))

	// Figures: the read tools over the rebuilt store against the CLI on files.
	for i, id := range replayed {
		for _, c := range []struct {
			tool string
			run  func([]string) (int, error)
		}{{"checkin", runCheckin}, {"ledger", noCode(runLedger)}, {"book", noCode(runBook)}} {
			o := cli(t, c.run, "--snapshot", "snapshots/"+id)
			got := callTool(t, cs, c.tool, map[string]any{"snapshot": id})
			if o.err != nil || got.isError || got.text != o.text || got.exitCode != o.code {
				t.Errorf("%s %s after replay: differs from the CLI", c.tool, id)
			}
		}
		from := "2026-02-06"
		if i > 0 {
			from = replayed[i-1]
		}
		if report := callTool(t, cs, "diff", map[string]any{"from": from, "to": id}); !report.isError {
			o := cli(t, noCode(runDiff), "--from", "snapshots/"+from, "--to", "snapshots/"+id)
			if report.text != o.text {
				t.Errorf("diff %s → %s after replay: differs from the CLI", from, id)
			}
		}
	}
}

func filesOf(t *testing.T, fsys fs.FS) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, top := range []string{"snapshots", "history", "reconcile"} {
		err := fs.WalkDir(fsys, top, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			out[p], err = fs.ReadFile(fsys, p)
			return err
		})
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
	}
	return out
}

// B2 through the server: nothing is added to a sealed snapshot, and
// history_add refuses before the log gains a line.
func TestWriteToolsRefuseSealedSnapshot(t *testing.T) {
	ctx := context.Background()
	cs, db, now := writeSession(t, before(t))
	checkinThroughServer(t, cs, now, "2026-02-13")
	logLen := func() int {
		items, _ := db.List(ctx, store.COrderVersion)
		return len(items)
	}
	n := logLen()

	// A pull with a genuinely new order (this one's id is not in the log) must still be refused.
	pull, _ := os.ReadFile("snapshots/2026-02-13/roth/orders_pull/window_p1.json")
	pull = []byte(strings.ReplaceAll(string(pull), "b0000007-0000-4000-8000-000000000007", "b00000ff-0000-4000-8000-0000000000ff"))
	if out := callTool(t, cs, "ingest", map[string]any{"snapshot": "2026-02-13", "role": "orders_pull", "account": "roth", "page": "late_p1",
		"call": "get_option_orders", "body": string(pull), "verbatim": false, "note": "test"}); !out.isError || !strings.Contains(out.text, "sealed") {
		t.Errorf("ingest into a sealed snapshot: %+v", out)
	}
	out := callTool(t, cs, "history_add", map[string]any{"snapshot": "2026-02-13", "account": "roth",
		"observed_at": "2026-02-14T15:00:00Z", "created_since": "2026-02-13", "pulls": [][]string{{"window_p1"}, {"open_p1"}}})
	if !out.isError || !strings.Contains(out.text, "nothing was appended") {
		t.Errorf("history_add on a sealed snapshot: %+v", out)
	}
	if logLen() != n {
		t.Errorf("the log grew from %d to %d lines on a refused history_add", n, logLen())
	}
	if _, err := store.Open(ctx, db); err != nil {
		t.Errorf("the seal no longer holds: %v", err)
	}
	// What may follow a seal still can.
	pnl, _ := os.ReadFile("reconcile/2026-02-13/roth_realized_pnl.json")
	mustTool(t, cs, "ingest", map[string]any{"snapshot": "2026-02-13", "role": "realized_pnl", "account": "roth",
		"call": "get_realized_pnl", "body": string(pnl), "verbatim": false, "note": "test"})
	if _, err := store.Open(ctx, db); err != nil {
		t.Errorf("a realized_pnl after the seal broke it: %v", err)
	}
}

// B1/B3 through the server: ids go through the one parser, and the clock
// (set to 2030 here) never dates a figure.
func TestWriteToolsDateByID(t *testing.T) {
	cs, _, now := writeSession(t, before(t))
	for _, bad := range []string{"latest", "2026-02-14-am", ""} {
		if out := callTool(t, cs, "open_snapshot", map[string]any{"snapshot": bad}); !out.isError {
			t.Errorf("open_snapshot %q accepted", bad)
		}
	}
	checkinThroughServer(t, cs, now, "2026-02-13")
	if out := callTool(t, cs, "open_snapshot", map[string]any{"snapshot": "2026-02-13"}); !out.isError {
		t.Error("a snapshot was opened twice")
	}
	out := mustTool(t, cs, "checkin", map[string]any{"snapshot": "2026-02-13"})
	if !strings.HasPrefix(out.text, "# Check-in as of 2026-02-13\n") {
		t.Errorf("checkin dated by something other than the id: %q", out.text[:40])
	}
}

// Stored responses never change, and a malformed one is refused.
func TestIngestRules(t *testing.T) {
	cs, _, _ := writeSession(t, before(t))
	mustTool(t, cs, "open_snapshot", map[string]any{"snapshot": "2026-02-13"})
	body, _ := os.ReadFile("snapshots/2026-02-13/roth/portfolio.json")
	ing := func(b string, extra map[string]any) toolOut {
		args := map[string]any{"snapshot": "2026-02-13", "role": "portfolio", "account": "roth", "call": "get_portfolio", "body": b, "verbatim": false, "note": "test"}
		for k, v := range extra {
			args[k] = v
		}
		return callTool(t, cs, "ingest", args)
	}
	if out := ing(string(body), nil); out.isError {
		t.Fatal(out.text)
	}
	if out := ing(string(body), nil); out.isError {
		t.Errorf("re-ingesting identical bytes: %s", out.text)
	}
	if out := ing(strings.Replace(string(body), "1", "2", 1), nil); !out.isError || !strings.Contains(out.text, "different contents") {
		t.Errorf("a changed response was accepted: %+v", out)
	}
	for name, extra := range map[string]map[string]any{
		"malformed":            {"role": "equity_quotes", "account": "", "call": "get_equity_quotes", "body": `{"data":`},
		"paginated":            {"role": "equity_positions", "call": "get_equity_positions", "body": `{"data":{"positions":[],"next":"cursor"}}`},
		"retyped without note": {"note": "", "body": `{"data":{}}`, "account": "trad"},
		"wrong call":           {"call": "get_equity_positions", "account": "trad"},
		"unknown account":      {"account": "ira3"},
		"unknown role":         {"role": "bank_statement"},
		"not opened":           {"snapshot": "2026-02-14"},
	} {
		if out := ing(string(body), extra); !out.isError {
			t.Errorf("%s: accepted", name)
		}
	}
}
