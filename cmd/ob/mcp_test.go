package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/branonm/options-book/internal/layout"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/branonm/options-book/internal/config"
	"github.com/branonm/options-book/internal/obmcp"
	"github.com/branonm/options-book/internal/report"
	"github.com/branonm/options-book/internal/rules"
	"github.com/branonm/options-book/internal/store"
)

// The store spec the CLI and the server both use in these tests.
const memSpec = "emulator=memory-test"

// mcpSession serves the ob-mcp tools over an in-memory MCP connection,
// against the repo imported into an in-memory store.
func mcpSession(t *testing.T) (*mcp.ClientSession, *store.Mem) {
	t.Helper()
	ctx := context.Background()
	db := store.NewMem()
	if _, err := store.ImportRepo(ctx, db, os.DirFS(".")); err != nil {
		t.Fatal(err)
	}
	bks, accts, asha := testBooks(t)
	server := mcp.NewServer(&mcp.Implementation{Name: "options-book", Version: "test"}, &mcp.ServerOptions{Instructions: obmcp.Instructions})
	obmcp.Register(server, &obmcp.Deps{
		Books: bks, DefaultBook: layout.DefaultBook, Accounts: accts, AccountsSHA: asha,
		DB: db, Spec: memSpec, Commit: "test", CacheTTL: time.Hour,
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
	testDBs[memSpec] = db
	t.Cleanup(func() { delete(testDBs, memSpec) })
	return cs, db
}

// testBooks loads every book in the repo, and accounts.json, the way the server
// compiles them in.
func testBooks(t *testing.T) (map[string]*obmcp.BookConfig, *config.Accounts, string) {
	t.Helper()
	ab, err := os.ReadFile(layout.AccountsConfig)
	if err != nil {
		t.Fatal(err)
	}
	accts, err := config.ParseAccounts(layout.AccountsConfig, ab)
	if err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(layout.BooksDir)
	if err != nil {
		t.Fatal(err)
	}
	bks := map[string]*obmcp.BookConfig{}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		bb, err := os.ReadFile(layout.BookFile(e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		rb, err := os.ReadFile(layout.RulesFile(e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := config.ParseBook(e.Name(), layout.BookFile(e.Name()), bb, accts)
		if err != nil {
			t.Fatal(err)
		}
		r, err := rules.Parse(layout.RulesFile(e.Name()), rb)
		if err != nil {
			t.Fatal(err)
		}
		bks[e.Name()] = &obmcp.BookConfig{Name: e.Name(), Cfg: cfg, Rules: r, BookSHA: store.SHA256(bb), RulesSHA: store.SHA256(rb)}
	}
	return bks, accts, store.SHA256(ab)
}

type toolOut struct {
	text     string
	exitCode int
	runID    string
	isError  bool
}

func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) toolOut {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var out toolOut
	out.isError = res.IsError
	content := ""
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			content += tc.Text
		}
	}
	// B7: the Claude connector shows structuredContent in place of the text
	// content. Read the result the way it does: the report (or the refusal)
	// must be in the structured output, identical to the text content, and
	// every comparison downstream uses what the structured output carries.
	if res.StructuredContent == nil {
		t.Fatalf("%s %v: no structured content; a client that shows structuredContent would see nothing (B7)", name, args)
	}
	b, _ := json.Marshal(res.StructuredContent)
	var r obmcp.Result
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("%s: structured content: %v", name, err)
	}
	seen := r.Output
	if res.IsError {
		seen = r.Error
	}
	if seen != content {
		t.Errorf("%s %v: the structured output doesn't carry the report (B7)\n--- structured\n%s\n--- content\n%s", name, args, seen, content)
	}
	if seen == "" {
		t.Errorf("%s %v: empty report", name, args)
	}
	out.text, out.exitCode, out.runID = seen, r.ExitCode, r.RunID
	return out
}

// cli runs a CLI command with stdout captured.
func cli(t *testing.T, f func([]string) (int, error), args ...string) cliOut {
	t.Helper()
	var b bytes.Buffer
	old := stdout
	stdout = &b
	defer func() { stdout = old }()
	code, err := f(args)
	return cliOut{b.String(), code, err}
}

type cliOut struct {
	text string
	code int
	err  error
}

func noCode(f func([]string) error) func([]string) (int, error) {
	return func(a []string) (int, error) { return 0, f(a) }
}

func snapshotIDs(t *testing.T) []string {
	t.Helper()
	dirs, _ := filepath.Glob("snapshots/2026-*")
	var ids []string
	for _, d := range dirs {
		ids = append(ids, filepath.Base(d))
	}
	if len(ids) < 2 { // a broken glob, not a count: the synthetic repo has two, the real one more
		t.Fatalf("only %d snapshots", len(ids))
	}
	return ids
}

// TestToolsMatchCLI is phase 3's gate: every tool's output is byte-identical
// to the CLI's on every snapshot. The CLI reads the files on disk; the tools
// read the store, over a real MCP session.
func TestToolsMatchCLI(t *testing.T) {
	cs, db := mcpSession(t)
	n, refused := 0, 0
	same := func(label string, c cliOut, got toolOut) {
		t.Helper()
		n++
		if c.err != nil {
			// The CLI refused: the tool must refuse with the same reason.
			refused++
			if !got.isError || !strings.Contains(got.text, c.err.Error()) {
				t.Errorf("%s: CLI error %q, tool %+v", label, c.err, got)
			}
			return
		}
		cliText, cliCode := c.text, c.code
		if got.isError {
			t.Errorf("%s: tool error: %s", label, got.text)
			return
		}
		if got.text != cliText {
			t.Errorf("%s: tool output differs from the CLI's\n--- cli\n%s\n--- tool\n%s", label, cliText, got.text)
		}
		if got.exitCode != cliCode {
			t.Errorf("%s: exit code %d, CLI %d", label, got.exitCode, cliCode)
		}
	}

	// Store-wide tools first, before any run documents change the export size.
	c := cli(t, noCode(runDB), "verify", "--emulator", "memory-test")
	same("verify", c, callTool(t, cs, "verify", nil))
	c = cli(t, noCode(runDB), "list", "--emulator", "memory-test")
	same("list_snapshots", c, callTool(t, cs, "list_snapshots", nil))

	ids := snapshotIDs(t)
	for _, id := range ids {
		dir := "snapshots/" + id
		for _, js := range []bool{false, true} {
			args := []string{"--snapshot", dir}
			if js {
				args = append(args, "--json")
			}
			c := cli(t, runCheckin, args...)
			same("checkin "+id+fmt.Sprint(js), c, callTool(t, cs, "checkin", map[string]any{"snapshot": id, "json": js}))
			c = cli(t, noCode(runLedger), args...)
			same("ledger "+id+fmt.Sprint(js), c, callTool(t, cs, "ledger", map[string]any{"snapshot": id, "json": js}))
			c = cli(t, noCode(runLedger), append(args, "--account", "trad")...)
			same("ledger trad "+id+fmt.Sprint(js), c, callTool(t, cs, "ledger", map[string]any{"snapshot": id, "account": "trad", "json": js}))
		}
		c := cli(t, noCode(runBook), "--snapshot", dir)
		same("book "+id, c, callTool(t, cs, "book", map[string]any{"snapshot": id}))

		raw := []string{"roth/equity_positions.json", "equity_quotes.json"}
		c = cli(t, noCode(runDB), "show", "--emulator", "memory-test", "--snapshot", id, "--raw", raw[0], "--raw", raw[1])
		same("get_snapshot "+id, c, callTool(t, cs, "get_snapshot", map[string]any{"snapshot": id, "raw": raw}))
	}
	for i := 1; i < len(ids); i++ {
		if err := report.CheckOrder(ids[i-1], ids[i]); err != nil {
			// A date-only and a timed id from one session can't be ordered; the
			// CLI refuses (PLAN.md known limits), so the tool must too.
			if out := callTool(t, cs, "diff", map[string]any{"from": ids[i-1], "to": ids[i]}); !out.isError {
				t.Errorf("diff %s → %s: the CLI refuses (%v) but the tool answered", ids[i-1], ids[i], err)
			}
			continue
		}
		c := cli(t, noCode(runDiff), "--from", "snapshots/"+ids[i-1], "--to", "snapshots/"+ids[i])
		same("diff "+ids[i], c, callTool(t, cs, "diff", map[string]any{"from": ids[i-1], "to": ids[i]}))
	}
	for _, js := range []bool{false, true} {
		args := []string{"--snapshot", "snapshots/2026-02-13", "--pnl", "roth=reconcile/2026-02-13/roth_realized_pnl.json", "--pnl", "trad=reconcile/2026-02-13/trad_realized_pnl.json"}
		if js {
			args = append(args, "--json")
		}
		c := cli(t, runReconcile, args...)
		same("reconcile "+fmt.Sprint(js), c, callTool(t, cs, "reconcile", map[string]any{"snapshot": "2026-02-13", "json": js}))
	}
	// The synthetic BBB $85c 3/20: at 2/06 BBB's lot is still covered (rejected),
	// at 2/13 it is free (accepted for roth), and trad holds no BBB (rejected).
	const bbbCall = "a0000000-0000-4000-8000-000000000021"
	tickets := []struct{ snap, acct, option string }{
		{"2026-02-06", "roth", bbbCall},
		{"2026-02-13", "roth", bbbCall},
		{"2026-02-13", "trad", bbbCall},
	}
	for _, tk := range tickets {
		for _, kind := range []string{"covered_call", "acquisition_put"} {
			for _, js := range []bool{false, true} {
				args := []string{"--snapshot", "snapshots/" + tk.snap, "--account", tk.acct, "--kind", kind, "--option", tk.option, "--limit", "0.80"}
				if js {
					args = append(args, "--json")
				}
				c := cli(t, runTicket, args...)
				same(fmt.Sprintf("ticket %v %s %v", tk, kind, js), c, callTool(t, cs, "ticket", map[string]any{
					"snapshot": tk.snap, "account": tk.acct, "kind": kind, "option": tk.option, "limit": "0.80", "json": js}))
			}
		}
	}
	t.Logf("%d tool calls matched the CLI: %d byte for byte, %d refused with the same reason", n, n-refused, refused)

	// Every figure-producing call left exactly one run document, and the
	// store still verifies with them in it.
	runs, err := db.List(context.Background(), store.CRun)
	if err != nil {
		t.Fatal(err)
	}
	figures := n - refused - 2 - len(ids) // less refusals, verify, list_snapshots and the get_snapshot calls
	if len(runs) != figures {
		t.Errorf("%d run documents for %d figure-producing calls", len(runs), figures)
	}
	if _, err := store.Verify(context.Background(), db); err != nil {
		t.Errorf("verify with run documents: %v", err)
	}
}

// A run document traces a figure to its inputs and output.
func TestRunDocument(t *testing.T) {
	cs, db := mcpSession(t)
	out := callTool(t, cs, "checkin", map[string]any{"snapshot": "2026-02-13"})
	if out.runID == "" {
		t.Fatal("no run id")
	}
	d, err := db.Get(context.Background(), store.CRun, out.runID)
	if err != nil {
		t.Fatal(err)
	}
	rulesBytes, _ := os.ReadFile(layout.RulesFile(layout.DefaultBook))
	accountsBytes, _ := os.ReadFile(layout.AccountsConfig)
	want := map[string]any{
		"tool": "checkin", "snapshot_id": "2026-02-13", "tool_commit": "test",
		"output_sha256": store.SHA256([]byte(out.text)), "rules_sha256": store.SHA256(rulesBytes),
		"args": `{"snapshot":"2026-02-13"}`, "exit_code": int64(out.exitCode),
		"book": layout.DefaultBook, "accounts_sha256": store.SHA256(accountsBytes),
	}
	for k, v := range want {
		if d[k] != v {
			t.Errorf("run.%s = %v, want %v", k, d[k], v)
		}
	}
}

// Errors come back as tool errors with the reason, and record no run.
func TestToolErrors(t *testing.T) {
	cs, db := mcpSession(t)
	for name, args := range map[string]map[string]any{
		"checkin":      {"snapshot": "2026-12-01"},
		"diff":         {"from": "2026-02-13", "to": "2026-02-06"},
		"ticket":       {"snapshot": "2026-02-13", "account": "roth", "kind": "covered_call", "option": "no-such-id", "limit": "0.80"},
		"reconcile":    {"snapshot": "2026-02-06"},
		"get_snapshot": {"snapshot": "2026-02-13", "raw": []string{"../2026-02-06/equity_quotes.json"}},
	} {
		out := callTool(t, cs, name, args)
		if !out.isError || out.text == "" {
			t.Errorf("%s %v: want a tool error, got %+v", name, args, out)
		}
	}
	runs, _ := db.List(context.Background(), store.CRun)
	if len(runs) != 0 {
		t.Errorf("failed calls recorded %d runs", len(runs))
	}
}

// The tool list is exactly the read and write sets: every write is a create,
// and nothing can reach a broker or place an order.
func TestToolList(t *testing.T) {
	cs, _ := mcpSession(t)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	if got, want := strings.Join(names, ","), "book,checkin,diff,get_snapshot,history_add,history_plan,ingest,ledger,list_snapshots,open_snapshot,reconcile,record_expiration,seal_snapshot,ticket,verify"; got != want {
		t.Errorf("tools: %s, want %s", got, want)
	}
}

// Item 6 step 2's gate: `history_plan` and `ob history plan` say the same
// thing for both accounts. Only the closing instruction differs by design (it
// names the CLI or the tool), so it is dropped from both.
func TestHistoryPlanMatchesCLI(t *testing.T) {
	cs, _ := mcpSession(t)
	body := func(s string) string {
		lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
		return strings.Join(lines[:len(lines)-1], "\n")
	}
	for _, acct := range []string{"roth", "trad"} {
		var b bytes.Buffer
		old := stdout
		stdout = &b
		err := runHistory([]string{"plan", "--account", acct})
		stdout = old
		if err != nil {
			t.Fatal(err)
		}
		tool := callTool(t, cs, "history_plan", map[string]any{"account": acct})
		if tool.isError || body(tool.text) != body(b.String()) || !strings.HasPrefix(tool.text, "Next pull for "+acct+" (log has ") {
			t.Errorf("%s: history_plan differs from `ob history plan`\n--- cli\n%s\n--- tool\n%s", acct, b.String(), tool.text)
		}
	}
}

// Item 6 step 3b on the server: a tool's book parameter picks the book, the
// default is ira-wheel, an unknown book is refused with the list, and the
// run records which book and which accounts.json it was computed against.
func TestBookParameter(t *testing.T) {
	cs, db := mcpSession(t)
	def := callTool(t, cs, "checkin", map[string]any{"snapshot": "2026-02-13"})
	named := callTool(t, cs, "checkin", map[string]any{"snapshot": "2026-02-13", "book": layout.DefaultBook})
	if def.isError || named.isError || def.text != named.text || def.exitCode != named.exitCode {
		t.Errorf("checkin differs with book=%s", layout.DefaultBook)
	}
	// Every tool that takes a book answers the same for the named default.
	for tool, args := range map[string]map[string]any{
		"ledger":    {"snapshot": "2026-02-13"},
		"book":      {"snapshot": "2026-02-13"},
		"diff":      {"from": "2026-02-06", "to": "2026-02-13"},
		"reconcile": {"snapshot": "2026-02-13"},
	} {
		a := callTool(t, cs, tool, args)
		withBook := map[string]any{"book": layout.DefaultBook}
		for k, v := range args {
			withBook[k] = v
		}
		b := callTool(t, cs, tool, withBook)
		if a.isError || b.isError || a.text != b.text {
			t.Errorf("%s differs with book=%s", tool, layout.DefaultBook)
		}
	}
	for _, tool := range []string{"checkin", "ledger", "book"} {
		o := callTool(t, cs, tool, map[string]any{"snapshot": "2026-02-13", "book": "nope"})
		if !o.isError || !strings.Contains(o.text, `no book "nope"`) || !strings.Contains(o.text, layout.DefaultBook) {
			t.Errorf("%s book=nope: %+v", tool, o)
		}
	}
	for _, tool := range []string{"seal_snapshot", "record_expiration"} {
		o := callTool(t, cs, tool, map[string]any{"snapshot": "2026-02-13", "book": "nope"})
		if !o.isError || !strings.Contains(o.text, `no book "nope"`) {
			t.Errorf("%s book=nope: %+v", tool, o)
		}
	}
	// A refusal records no run; the ones that ran name their book.
	runs, _ := db.List(context.Background(), store.CRun)
	for _, r := range runs {
		if r.Doc["book"] != layout.DefaultBook {
			t.Errorf("run %s: book %v", r.ID, r.Doc["book"])
		}
	}
	if len(runs) < 6 {
		t.Errorf("%d runs recorded", len(runs))
	}
	// Writes name accounts from accounts.json, whichever book covers them.
	if o := callTool(t, cs, "history_plan", map[string]any{"account": "taxable"}); o.isError && strings.Contains(o.text, "no account") {
		t.Errorf("an account in accounts.json was refused: %s", o.text)
	}
	if o := callTool(t, cs, "history_plan", map[string]any{"account": "ira3"}); !o.isError || !strings.Contains(o.text, layout.AccountsConfig) {
		t.Errorf("an unknown account: %+v", o)
	}
}
