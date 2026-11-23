package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/branonm/options-book/internal/layout"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/branonm/options-book/internal/cred"
	"github.com/branonm/options-book/internal/obmcp"
	"github.com/branonm/options-book/internal/rhmcp"
	"github.com/branonm/options-book/internal/store"
)

// fakeRH answers Robinhood's MCP calls with the bodies a real snapshot
// saved, so fetch_snapshot can be checked against the file run offline.
type fakeRH struct {
	t     *testing.T
	snap  fs.FS // snapshots/<id>
	mu    sync.Mutex
	calls []string
	fail  string // a tool to answer with isError
}

// bbbCallID is the synthetic BBB $85c 2026-03-20, saved in snapshot 2026-02-13 for a ticket.
const bbbCallID = "a0000000-0000-4000-8000-000000000021"

var fakeNumbers = map[string]string{"roth": "700000001", "trad": "700000002"}

func (f *fakeRH) acct(args map[string]any) string {
	for name, n := range fakeNumbers {
		if args["account_number"] == n {
			return name
		}
	}
	f.t.Errorf("unknown account_number %v", args["account_number"])
	return ""
}

func (f *fakeRH) body(tool string, args map[string]any) (string, bool) {
	read := func(p string) string {
		b, err := fs.ReadFile(f.snap, p)
		if err != nil {
			f.t.Errorf("%s %v: no saved body %s", tool, args, p)
		}
		return string(b)
	}
	if tool == f.fail {
		return "rate limited", true
	}
	switch tool {
	case "get_accounts":
		return fmt.Sprintf(`{"data":{"accounts":[{"account_number":%q,"type":"ira_roth"},{"account_number":%q,"type":"ira_traditional"}]},"guide":"fake"}`, fakeNumbers["roth"], fakeNumbers["trad"]), false
	case "get_option_orders":
		a := f.acct(args)
		switch {
		case args["created_at_gte"] != nil:
			return read(a + "/orders_pull/window_p1.json"), false
		case args["state"] == "confirmed":
			return read(a + "/orders_pull/open_p1.json"), false
		case args["order_id"] != nil:
			id := fmt.Sprint(args["order_id"])
			return read(a + "/orders_pull/lookup_" + id[:8] + ".json"), false
		}
	case "get_equity_positions":
		return read(f.acct(args) + "/equity_positions.json"), false
	case "get_option_positions":
		if args["nonzero"] != true {
			f.t.Errorf("get_option_positions without nonzero=true")
		}
		return read(f.acct(args) + "/option_positions.json"), false
	case "get_portfolio":
		return read(f.acct(args) + "/portfolio.json"), false
	case "get_equity_quotes":
		return read("equity_quotes.json"), false
	case "get_option_quotes":
		if ids, _ := args["instrument_ids"].([]any); len(ids) == 1 && ids[0] == bbbCallID {
			return read("options/" + bbbCallID + "/quote.json"), false
		}
		return read("option_quotes.json"), false
	case "get_option_chains":
		if args["chain_symbol"] != "BBB" {
			f.t.Errorf("get_option_chains %v", args)
		}
		return `{"data":{"chains":[{"id":"b0000000-0000-4000-8000-000000000021","symbol":"BBB","expiration_dates":["2026-02-20","2026-03-20","2027-03-19"]}]}}`, false
	case "get_option_instruments":
		if args["chain_symbol"] != "BBB" || args["type"] != "call" || fmt.Sprint(args["expiration_dates"]) != "[2026-03-20]" {
			f.t.Errorf("get_option_instruments %v", args)
		}
		return read("options/" + bbbCallID + "/instrument.json"), false
	case "get_equity_fundamentals":
		return `{"data":{"results":[{"symbol":"BBB","pe_ratio":"12.30"}]},"guide":"Fundamentals are delayed."}`, false
	}
	return "no such tool", true
}

func (f *fakeRH) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer rh-access" {
		w.WriteHeader(401)
		return
	}
	var req struct {
		ID     *int            `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if req.ID == nil {
		w.WriteHeader(202)
		return
	}
	var result any
	switch req.Method {
	case "initialize":
		result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}}
	case "tools/list":
		var tools []any
		props := map[string]map[string]any{
			"get_option_chains":      {"chain_symbol": map[string]any{"type": "string"}},
			"get_option_instruments": {"chain_symbol": map[string]any{"type": "string"}, "expiration_dates": map[string]any{"type": "array"}, "type": map[string]any{"type": "string"}},
		}
		for _, n := range append(append(append([]string{}, rhmcp.Allowlist...), rhmcp.AdHoc...), "place_option_order", "cancel_option_order", "search") {
			p := props[n]
			if p == nil {
				p = map[string]any{}
			}
			tools = append(tools, map[string]any{"name": n, "inputSchema": map[string]any{"properties": p}})
		}
		result = map[string]any{"tools": tools}
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		json.Unmarshal(req.Params, &p)
		f.mu.Lock()
		f.calls = append(f.calls, p.Name)
		f.mu.Unlock()
		text, isErr := f.body(p.Name, p.Arguments)
		result = map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}, "isError": isErr}
	}
	json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": result})
}

// fetchNow is the server's clock in fetch sessions; tests may move it.
var fetchNow time.Time

// fetchSession serves every tool, fetch_snapshot included, against a store
// holding fsys and a fake Robinhood answering from snapshots/<from>.
func fetchSession(t *testing.T, fsys fs.FS, from string) (*mcp.ClientSession, store.DB, *fakeRH) {
	t.Helper()
	ctx := context.Background()
	db := store.NewMem()
	if _, err := store.ImportRepo(ctx, db, fsys); err != nil {
		t.Fatal(err)
	}
	bks, accts, asha := testBooks(t)
	f := &fakeRH{t: t, snap: os.DirFS("snapshots/" + from)}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)

	fetchNow = time.Date(2026, 2, 13, 15, 0, 0, 0, time.UTC) // 10:00 ET on 2/13
	clock := func() time.Time { return fetchNow }
	v := cred.NewMemVault()
	sec, _ := json.Marshal(cred.RobinhoodSecret{
		ClientID: "c", AccessToken: "rh-access", RefreshToken: "rh-refresh",
		AccessExpiresAt: fetchNow.Add(5 * 24 * time.Hour).Format(time.RFC3339),
		TokenEndpoint:   cred.RobinhoodToken, Resource: srv.URL, Scope: "internal",
	})
	v.Add(ctx, "robinhood-oauth", sec)

	server := mcp.NewServer(&mcp.Implementation{Name: "options-book", Version: "test"}, nil)
	obmcp.Register(server, &obmcp.Deps{
		Books: bks, DefaultBook: layout.DefaultBook, Accounts: accts, AccountsSHA: asha, DB: db, Spec: "memory", Commit: "test", CacheTTL: time.Hour, Now: clock,
		Vault: v, Robinhood: cred.NewRobinhood(nil, clock), AlpacaPaper: cred.NewAlpacaPaper(nil),
		RobinhoodMCP: srv.URL, RobinhoodHTTP: srv.Client(),
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
	return cs, db, f
}

// Phase 5's gate, offline: from the store as it stood at 2026-02-06, a
// fetch_snapshot whose Robinhood answers with 2/13's saved responses gives
// checkin, ledger and book output byte-identical to the file run's 2/13.
func TestFetchSnapshotMatchesFileRun(t *testing.T) {
	ctx := context.Background()
	cs, db, f := fetchSession(t, repoAt(t, "2026-02-06", nil), "2026-02-13")
	out := callTool(t, cs, "fetch_snapshot", map[string]any{"snapshot": "2026-02-13", "note": "test"})
	if out.isError {
		t.Fatalf("fetch_snapshot: %s", out.text)
	}
	for _, want := range []string{"Sealed snapshot 2026-02-13", "5.0 days left", "lookup_b0000006", "••0001"} {
		if !strings.Contains(out.text, want) {
			t.Errorf("report lacks %q:\n%s", want, out.text)
		}
	}
	for _, leak := range []string{"rh-access", "rh-refresh", "700000001", "700000002"} {
		if strings.Contains(out.text, leak) {
			t.Errorf("report shows %s", leak)
		}
	}
	for _, c := range f.calls {
		if !rhmcp.Allowed(c) {
			t.Errorf("called %s", c)
		}
	}

	for _, c := range []struct {
		tool string
		run  func([]string) (int, error)
	}{{"checkin", runCheckin}, {"ledger", noCode(runLedger)}, {"book", noCode(runBook)}} {
		o := cli(t, c.run, "--snapshot", "snapshots/2026-02-13")
		got := callTool(t, cs, c.tool, map[string]any{"snapshot": "2026-02-13"})
		if o.err != nil || got.isError || got.text != o.text || got.exitCode != o.code {
			t.Errorf("%s 2026-02-13 by fetch_snapshot differs from the file run", c.tool)
		}
	}

	// Every input is the saved response byte for byte. Bodies are content-addressed,
	// so one already stored (e.g. unchanged positions) keeps its first metadata;
	// every body first stored by this pull is recorded as a verbatim server pull.
	src, err := store.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	disk := os.DirFS("snapshots/2026-02-13")
	n := 0
	fs.WalkDir(disk, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		// Ticket contracts (options/) are added by hand for a ticket; fetch_snapshot
		// pulls only what a check-in needs. PROVENANCE.md is generated, and expected.json
		// is the hand-computed expectation a real fetch has no way to pull.
		if _, ok, _ := store.MemberFor("2026-02-13", p); !ok || p == "PROVENANCE.md" || p == layout.Expected || strings.HasPrefix(p, "options/") {
			return nil
		}
		want, _ := fs.ReadFile(disk, p)
		got, err := fs.ReadFile(src.FS, "snapshots/2026-02-13/"+p)
		if err != nil || string(got) != string(want) {
			t.Errorf("%s: not the saved bytes", p)
		}
		n++
		return nil
	})
	raws, _ := db.List(ctx, store.CRawResponse)
	server := 0
	for _, it := range raws {
		if it.Doc["source"] == obmcp.RobinhoodSource {
			server++
			if it.Doc["verbatim"] != true || it.Doc["fetched_at"] == "" || it.Doc["args"] == "null" {
				t.Errorf("raw_response %s: %v", it.ID[:12], it.Doc)
			}
		}
	}
	if server == 0 {
		t.Error("no body was recorded as a server pull")
	}
	t.Logf("%d inputs byte-identical; %d new bodies recorded as server pulls; %d Robinhood calls", n, server, len(f.calls))
}

// A failure after opening leaves the snapshot open and unsealed, says so,
// and stores nothing for the failed call.
func TestFetchSnapshotPartialFailure(t *testing.T) {
	ctx := context.Background()
	cs, db, f := fetchSession(t, repoAt(t, "2026-02-06", nil), "2026-02-13")
	f.fail = "get_option_quotes"
	out := callTool(t, cs, "fetch_snapshot", map[string]any{"snapshot": "2026-02-13"})
	if !out.isError || !strings.Contains(out.text, "rate limited") || !strings.Contains(out.text, "left open") || !strings.Contains(out.text, "NEW id") {
		t.Fatalf("partial failure: %+v", out)
	}
	if ok, _ := store.IsSealed(ctx, db, "2026-02-13"); ok {
		t.Error("a partial snapshot was sealed")
	}
	if again := callTool(t, cs, "fetch_snapshot", map[string]any{"snapshot": "2026-02-13"}); !again.isError || !strings.Contains(again.text, "already exists") {
		t.Errorf("retry under the same id: %+v", again)
	}
	// A dead token fails before anything is opened.
	f.fail = "get_accounts"
	out = callTool(t, cs, "fetch_snapshot", map[string]any{"snapshot": "2026-02-14"})
	if !out.isError || !strings.Contains(out.text, "nothing was stored") {
		t.Errorf("failure before opening: %+v", out)
	}
	if _, err := db.Get(ctx, store.CSnapshot, "2026-02-14"); err == nil {
		t.Error("a snapshot was opened though nothing could be pulled")
	}
}

func TestCredStatusTool(t *testing.T) {
	cs, _, _ := fetchSession(t, repoAt(t, "2026-02-06", nil), "2026-02-13")
	out := callTool(t, cs, "cred_status", nil)
	if !strings.Contains(out.text, "robinhood: secret robinhood-oauth version 1") || !strings.Contains(out.text, "5.0 days left") ||
		!strings.Contains(out.text, "alpaca-paper: not enrolled") || strings.Contains(out.text, "rh-access") {
		t.Errorf("cred_status: %q", out.text)
	}
}

// rh_read: one read-only call, stored verbatim with no snapshot, returned as sent.
func TestRHRead(t *testing.T) {
	ctx := context.Background()
	cs, db, f := fetchSession(t, repoAt(t, "2026-02-06", nil), "2026-02-13")
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "rh_read", Arguments: map[string]any{
		"tool": "get_equity_fundamentals", "args": map[string]any{"symbols": []string{"BBB"}}}})
	if err != nil || res.IsError {
		t.Fatalf("rh_read: %v %v", err, res)
	}
	out := callTool(t, cs, "rh_read", map[string]any{"tool": "get_equity_fundamentals", "args": map[string]any{"symbols": []string{"BBB"}}})
	want := `{"data":{"results":[{"symbol":"BBB","pe_ratio":"12.30"}]},"guide":"Fundamentals are delayed."}`
	if out.text != want {
		t.Errorf("returned %q", out.text)
	}
	b, _ := json.Marshal(res.StructuredContent)
	var r obmcp.Result
	json.Unmarshal(b, &r)
	if r.SHA256 != store.SHA256([]byte(want)) {
		t.Errorf("sha256 %s", r.SHA256)
	}
	d, err := db.Get(ctx, store.CRawResponse, r.SHA256)
	if err != nil || d["source"] != obmcp.RobinhoodSource || d["call"] != "get_equity_fundamentals" || d["args"] != `{"symbols":["BBB"]}` || d["verbatim"] != true {
		t.Errorf("stored: %v %v", d, err)
	}
	if body, err := store.RawBody(ctx, db, r.SHA256); err != nil || string(body) != want {
		t.Errorf("read back: %q %v", body, err)
	}
	// Nothing outside the read lists reaches Robinhood, even though it offers them.
	before := len(f.calls)
	for _, tool := range []string{"place_option_order", "cancel_option_order", "search", "get_watchlists"} {
		if o := callTool(t, cs, "rh_read", map[string]any{"tool": tool}); !o.isError || !strings.Contains(o.text, "not a read-only tool") {
			t.Errorf("rh_read %s: %+v", tool, o)
		}
	}
	if len(f.calls) != before {
		t.Errorf("refused tools reached Robinhood: %v", f.calls[before:])
	}
	// The store still verifies with stand-alone bodies in it.
	if _, err := store.Verify(ctx, db); err != nil {
		t.Errorf("verify: %v", err)
	}
}

// Addendum A: ticket by OCC contract. The server resolves the contract
// through Robinhood, stores each response, and computes the ticket from the
// stored bytes; the ticket matches `ob ticket` on the same contract and
// positions, and the quote window is judged by the server's own fetch time.
func TestTicketByContract(t *testing.T) {
	ctx := context.Background()
	cs, db, _ := fetchSession(t, repoAt(t, "2026-02-13", nil), "2026-02-13")
	args := map[string]any{"contract": "BBB   260320C00085000", "account": "roth", "kind": "covered_call", "limit": "1.25", "snapshot": "2026-02-13"}
	out := callTool(t, cs, "ticket", args)
	if out.isError {
		t.Fatalf("ticket: %s", out.text)
	}
	header, body, ok := strings.Cut(out.text, "\n\n# Ticket:")
	if !ok {
		t.Fatalf("no ticket in %q", out.text)
	}
	o := cli(t, runTicket, "--snapshot", "snapshots/2026-02-13", "--account", "roth", "--kind", "covered_call", "--option", bbbCallID, "--limit", "1.25")
	if o.err != nil || "# Ticket:"+body != o.text || out.exitCode != o.code {
		t.Errorf("ticket by contract differs from `ob ticket` on the stored contract\n--- cli\n%s\n--- tool\n# Ticket:%s", o.text, body)
	}
	for _, want := range []string{"Contract BBB   260320C00085000: instrument " + bbbCallID, "fetched by this server", "stored get_option_chains", "stored get_option_instruments", "stored get_option_quotes", "snapshot 2026-02-13"} {
		if !strings.Contains(header, want) {
			t.Errorf("header lacks %q:\n%s", want, header)
		}
	}
	// The run records the three stored responses it read.
	d, err := db.Get(ctx, store.CRun, out.runID)
	if err != nil || strings.Count(fmt.Sprint(d["inputs"]), ",") != 2 || d["snapshot_id"] != "2026-02-13" {
		t.Errorf("run: %v %v", d, err)
	}
	// Each input is stored. Bodies are content-addressed, so bytes already stored
	// (here, the hand-saved files the import holds) keep their first metadata.
	var calls []string
	for _, sha := range strings.Split(fmt.Sprint(d["inputs"]), ",") {
		raw, err := db.Get(ctx, store.CRawResponse, sha)
		if err != nil {
			t.Errorf("input %s: %v", sha, err)
			continue
		}
		calls = append(calls, fmt.Sprint(raw["call"]))
	}
	if strings.Join(calls, ",") != "get_option_chains,get_option_instruments,get_option_quotes" {
		t.Errorf("inputs are %v", calls)
	}

	// Without a snapshot: the latest sealed one.
	delete(args, "snapshot")
	if o := callTool(t, cs, "ticket", args); o.isError || !strings.Contains(o.text, "snapshot 2026-02-13, the latest sealed") {
		t.Errorf("default snapshot: %s", o.text)
	}
	// JSON carries the server's fetch time.
	args["json"] = true
	if o := callTool(t, cs, "ticket", args); o.isError || !strings.Contains(o.text, `"fetched_at": "2026-02-13T15:00:00Z"`) {
		t.Errorf("json: %s", o.text)
	}
	delete(args, "json")
	// After the close, the server's own clock says so; no time from the session counts.
	fetchNow = time.Date(2026, 2, 13, 21, 30, 0, 0, time.UTC) // 16:30 ET
	if o := callTool(t, cs, "ticket", args); o.exitCode != 1 || !strings.Contains(o.text, "fetched after the 16:00 close") {
		t.Errorf("after hours: %d %s", o.exitCode, o.text)
	}
	args["asof"] = "2026-02-13"
	if o := callTool(t, cs, "ticket", args); !o.isError || !strings.Contains(o.text, "give neither option nor asof") {
		t.Errorf("a session-supplied asof was accepted: %+v", o)
	}
	delete(args, "asof")
	args["contract"] = "BBB $85c 2026-03-20"
	if o := callTool(t, cs, "ticket", args); !o.isError || !strings.Contains(o.text, "not an OCC symbol") {
		t.Errorf("bad contract: %+v", o)
	}
}

// recommend: the server keeps the chain's expirations inside the DTE window
// (2/20 is 7 days out and 3/19/27 is 399, so only 3/20 survives), pulls the
// calls there and their quotes, stores each response, and runs every contract
// through the ticket checks.
func TestRecommendTool(t *testing.T) {
	ctx := context.Background()
	cs, db, _ := fetchSession(t, repoAt(t, "2026-02-13", nil), "2026-02-13")
	out := callTool(t, cs, "recommend", map[string]any{"symbol": "bbb", "account": "roth", "snapshot": "2026-02-13"})
	if out.isError {
		t.Fatalf("recommend: %s", out.text)
	}
	for _, want := range []string{"BBB calls 14–240 DTE", "fetched by this server", "snapshot 2026-02-13", "# roth BBB", "Considered 1 contracts"} {
		if !strings.Contains(out.text, want) {
			t.Errorf("output lacks %q:\n%s", want, out.text)
		}
	}
	d, err := db.Get(ctx, store.CRun, out.runID)
	if err != nil || d["tool"] != "recommend" || strings.Count(fmt.Sprint(d["inputs"]), ",") != 2 {
		t.Errorf("run: %v %v", d, err)
	}
	// A window that ends before it starts is refused before anything is fetched.
	if o := callTool(t, cs, "recommend", map[string]any{"symbol": "bbb", "min_dte": 100, "max_dte": 50, "snapshot": "2026-02-13"}); !o.isError || !strings.Contains(o.text, "under min_dte") {
		t.Errorf("window: %v %s", o.isError, o.text)
	}
	// No shares, no recommendation.
	if o := callTool(t, cs, "recommend", map[string]any{"symbol": "ZZZ", "snapshot": "2026-02-13"}); !o.isError || !strings.Contains(o.text, "holds ZZZ") {
		t.Errorf("ZZZ: %v %s", o.isError, o.text)
	}
}
