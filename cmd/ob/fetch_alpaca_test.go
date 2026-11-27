package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/branonm/options-book/internal/alpacapull"
	"github.com/branonm/options-book/internal/config"
	"github.com/branonm/options-book/internal/cred"
	"github.com/branonm/options-book/internal/obmcp"
	"github.com/branonm/options-book/internal/report"
	"github.com/branonm/options-book/internal/rules"
	"github.com/branonm/options-book/internal/snapshot"
	"github.com/branonm/options-book/internal/store"
)

// paperRoot and alpacaSamps live beside the code, not in the data root the
// tests run from; TestMain sets them from moduleDir.
var paperRoot, alpacaSamps string

const closerID = "6c1d0a10-0000-4000-8000-000000000002"

// fakeAlpaca answers Alpaca's REST paths from the synthetic samples, and
// records every request. Requests reach it by a transport that rewrites the
// host, so the allowlist and the credential pin run against the real hosts.
type fakeAlpaca struct {
	t     *testing.T
	mu    sync.Mutex
	calls []string
	open  string // the body for status=open
	drop  string // an order id left out of windowed (after=) pulls, so only a lookup finds it
	acct  string // when set, the body served for /v2/account instead of the sample
	empty bool   // a brand-new account: no orders, no positions
}

func (f *fakeAlpaca) read(name string) string {
	b, err := os.ReadFile(filepath.Join(alpacaSamps, name))
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b)
}

func (f *fakeAlpaca) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := r.Header.Get("X-Real-Host")
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+host+r.URL.RequestURI())
	f.mu.Unlock()
	if r.Header.Get("APCA-API-KEY-ID") != "PKTEST" || r.Header.Get("APCA-API-SECRET-KEY") != "sekret" {
		w.WriteHeader(401)
		return
	}
	switch {
	case r.URL.Path == "/v2/account":
		if f.acct != "" {
			w.Write([]byte(f.acct))
			return
		}
		w.Write([]byte(f.read("account.json")))
	case r.URL.Path == "/v2/positions":
		if f.empty {
			w.Write([]byte(`[]`))
			return
		}
		w.Write([]byte(f.read("positions.json")))
	case r.URL.Path == "/v2/orders" && r.URL.Query().Get("status") == "open":
		w.Write([]byte(f.open))
	case r.URL.Path == "/v2/orders":
		if f.empty {
			w.Write([]byte(`[]`))
			return
		}
		body := f.read("orders.json")
		if f.drop != "" && r.URL.Query().Get("after") != "" {
			var all []map[string]any
			json.Unmarshal([]byte(body), &all)
			kept := all[:0]
			for _, o := range all {
				if o["id"] != f.drop {
					kept = append(kept, o)
				}
			}
			b, _ := json.Marshal(kept)
			body = string(b)
		}
		w.Write([]byte(body))
	case strings.HasPrefix(r.URL.Path, "/v2/orders/"):
		var all []map[string]any
		json.Unmarshal([]byte(f.read("orders.json")), &all)
		for _, o := range all {
			if "/v2/orders/"+o["id"].(string) == r.URL.Path {
				json.NewEncoder(w).Encode(o)
				return
			}
		}
		w.WriteHeader(404)
	case r.URL.Path == "/v2/stocks/trades/latest":
		w.Write([]byte(f.read("equity_trades.json")))
	case r.URL.Path == "/v2/options/contracts":
		w.Write([]byte(f.read("contracts.json")))
	case r.URL.Path == "/v1beta1/options/snapshots":
		w.Write([]byte(f.read("option_snapshots.json")))
	default:
		w.WriteHeader(404)
	}
}

type hostRewrite struct{ to *url.URL }

func (h hostRewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	c := req.Clone(req.Context())
	c.Header.Set("X-Real-Host", req.URL.Host)
	c.URL.Scheme, c.URL.Host = h.to.Scheme, h.to.Host
	return http.DefaultTransport.RoundTrip(c)
}

func paperBook(t *testing.T) (map[string]*obmcp.BookConfig, *config.Accounts, string) {
	t.Helper()
	ab, err := os.ReadFile(filepath.Join(paperRoot, "config", "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	accts, err := config.ParseAccounts("accounts.json", ab)
	if err != nil {
		t.Fatal(err)
	}
	bb, _ := os.ReadFile(filepath.Join(paperRoot, "books", "paper-wheel", "book.json"))
	rb, _ := os.ReadFile(filepath.Join(paperRoot, "books", "paper-wheel", "rules.json"))
	cfg, err := config.ParseBook("paper-wheel", "book.json", bb, accts)
	if err != nil {
		t.Fatal(err)
	}
	r, err := rules.Parse("rules.json", rb)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]*obmcp.BookConfig{"paper-wheel": {Name: "paper-wheel", Cfg: cfg, Rules: r, BookSHA: store.SHA256(bb), RulesSHA: store.SHA256(rb)}}, accts, store.SHA256(ab)
}

func alpacaSession(t *testing.T, f *fakeAlpaca) (*mcp.ClientSession, *time.Time) {
	t.Helper()
	ctx := context.Background()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	now := time.Date(2026, 11, 10, 15, 35, 0, 0, time.UTC) // 10:35 ET
	clock := func() time.Time { return now }
	v := cred.NewMemVault()
	sec, _ := json.Marshal(cred.AlpacaPaperSecret{KeyID: "PKTEST", SecretKey: "sekret", Endpoint: cred.AlpacaPaperEndpoint, EnrolledAt: now.Format(time.RFC3339)})
	v.Add(ctx, "alpaca-paper", sec)
	bks, accts, asha := paperBook(t)
	server := mcp.NewServer(&mcp.Implementation{Name: "options-book", Version: "test"}, nil)
	obmcp.Register(server, &obmcp.Deps{
		Books: bks, DefaultBook: "paper-wheel", Accounts: accts, AccountsSHA: asha, DB: store.NewMem(), Spec: "memory", Commit: "test", CacheTTL: time.Hour, Now: clock,
		Vault: v, AlpacaPaper: cred.NewAlpacaPaper(nil), AlpacaHTTP: &http.Client{Transport: hostRewrite{u}},
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
	return cs, &now
}

// The Fetcher's gate, offline: a fetch_snapshot against a fake Alpaca stores
// every response verbatim, and checkin through the server equals the
// file-based paper run on the same data.
func TestFetchSnapshotAlpaca(t *testing.T) {
	f := &fakeAlpaca{t: t, open: `[]`}
	cs, now := alpacaSession(t, f)

	out := callTool(t, cs, "fetch_snapshot", map[string]any{"book": "paper-wheel", "snapshot": "2026-11-10"})
	if out.isError {
		t.Fatalf("fetch_snapshot: %s", out.text)
	}
	for _, want := range []string{"Sealed snapshot 2026-11-10", "full history pull", "appended 5 lines"} {
		if !strings.Contains(out.text, want) {
			t.Errorf("report lacks %q:\n%s", want, out.text)
		}
	}
	for _, leak := range []string{"PKTEST", "sekret"} {
		if strings.Contains(out.text, leak) {
			t.Errorf("report shows %s", leak)
		}
	}
	// Every request was a GET to an allowlisted path.
	for _, c := range f.calls {
		method, rest, _ := strings.Cut(c, " ")
		host, p, _ := strings.Cut(rest, "/")
		p, _, _ = strings.Cut("/"+p, "?")
		if method != "GET" || !alpacapull.Allowed(host, p) {
			t.Errorf("request %s", c)
		}
	}

	// checkin through the server equals the file run over the fixture.
	cfg, r, at := paperFixture2(t)
	var want bytes.Buffer
	code, err := report.Checkin(&want, cfg, r, at, time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC), false)
	if err != nil {
		t.Fatal(err)
	}
	got := callTool(t, cs, "checkin", map[string]any{"book": "paper-wheel", "snapshot": "2026-11-10"})
	if got.isError || got.text != want.String() || got.exitCode != code {
		t.Errorf("checkin through the server differs from the file run:\n--- server\n%s\n--- files\n%s", got.text, want.String())
	}

	// A later snapshot is incremental: window, open list, and a lookup for the
	// log-open closer that the (empty) open list doesn't show.
	f.calls, f.drop = nil, closerID
	*now = now.Add(30 * time.Minute)
	out = callTool(t, cs, "fetch_snapshot", map[string]any{"book": "paper-wheel", "snapshot": "2026-11-10T1600"})
	if out.isError {
		t.Fatalf("second fetch: %s", out.text)
	}
	joined := strings.Join(f.calls, "\n")
	for _, want := range []string{"status=open", "/v2/orders/" + closerID, "after=2026-11-09T00%3A00%3A00Z"} {
		if !strings.Contains(joined, want) {
			t.Errorf("incremental pull lacks %q:\n%s", want, joined)
		}
	}
	if !strings.Contains(out.text, "0 new orders") || !strings.Contains(out.text, "lookup_6c1d0a10") {
		t.Errorf("incremental report:\n%s", out.text)
	}
}

func paperFixture2(t *testing.T) (*config.Book, *rules.Rules, snapshot.At) {
	t.Helper()
	cfg, err := config.Load(paperRoot, "paper-wheel")
	if err != nil {
		t.Fatal(err)
	}
	r, err := rules.Load(filepath.Join(paperRoot, "books", "paper-wheel", "rules.json"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg, r, snapshot.OnDisk(filepath.Join(paperRoot, "snapshots", "2026-11-10"))
}

// Open interest on Alpaca lives on the contract record, not the quote. A ticket
// by contract fetches both, stores each verbatim, joins them, and so sees the
// contract's open interest (14,460 in the sample), not 0.
func TestTicketByContractAlpaca(t *testing.T) {
	f := &fakeAlpaca{t: t, open: `[]`}
	cs, _ := alpacaSession(t, f)
	if out := callTool(t, cs, "fetch_snapshot", map[string]any{"book": "paper-wheel", "snapshot": "2026-11-10"}); out.isError {
		t.Fatalf("fetch_snapshot: %s", out.text)
	}
	f.calls = nil
	out := callTool(t, cs, "ticket", map[string]any{"book": "paper-wheel", "contract": "AAA   270617C00055000", "account": "paper", "kind": "covered_call", "limit": "2.30", "snapshot": "2026-11-10"})
	if out.isError {
		t.Fatalf("ticket: %s", out.text)
	}
	for _, want := range []string{"quoted by Alpaca", "stored GET /v2/options/contracts", "stored GET /v1beta1/options/snapshots", "OI 14460"} {
		if !strings.Contains(out.text, want) {
			t.Errorf("ticket lacks %q:\n%s", want, out.text)
		}
	}
	if strings.Contains(out.text, "open interest 0 is below") {
		t.Errorf("open interest read as 0:\n%s", out.text)
	}
	if len(f.calls) != 2 {
		t.Errorf("want one contracts and one snapshots request, got %v", f.calls)
	}
	for _, c := range f.calls {
		method, rest, _ := strings.Cut(c, " ")
		host, p, _ := strings.Cut(rest, "/")
		p, _, _ = strings.Cut("/"+p, "?")
		if method != "GET" || !alpacapull.Allowed(host, p) {
			t.Errorf("request %s", c)
		}
	}
}

// The account's last4 comes from config/accounts.json: a key whose account
// number doesn't end in it, or a response with no account number, is refused
// before a snapshot is opened, and the error never shows the full number.
func TestFetchSnapshotAlpacaChecksLast4(t *testing.T) {
	for name, tc := range map[string]struct{ body, want string }{
		"wrong account":     {`{"account_number":"PA1234569999","status":"ACTIVE"}`, "ends in 9999"},
		"no account number": {`{"status":"ACTIVE"}`, "no account_number"},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeAlpaca{t: t, open: `[]`, acct: tc.body}
			cs, _ := alpacaSession(t, f)
			out := callTool(t, cs, "fetch_snapshot", map[string]any{"book": "paper-wheel", "snapshot": "2026-11-10"})
			if !out.isError || !strings.Contains(out.text, tc.want) || !strings.Contains(out.text, "last4 0000") {
				t.Fatalf("want an error with %q and last4 0000, got error=%v:\n%s", tc.want, out.isError, out.text)
			}
			if strings.Contains(out.text, "PA123456") {
				t.Errorf("error shows the full account number:\n%s", out.text)
			}
		})
	}
}

// B9: a brand-new account has no orders. Its snapshot records position 0 of the
// empty order log instead of failing, seals, and verifies; the next pull is
// incremental from that observation (B12); and the first order lands as line 1.
func TestFetchSnapshotAlpacaEmptyAccount(t *testing.T) {
	f := &fakeAlpaca{t: t, open: `[]`, empty: true}
	cs, now := alpacaSession(t, f)

	out := callTool(t, cs, "fetch_snapshot", map[string]any{"book": "paper-wheel", "snapshot": "2026-11-10"})
	if out.isError {
		t.Fatalf("fetch_snapshot on an empty account: %s", out.text)
	}
	for _, want := range []string{"Sealed snapshot 2026-11-10", "full history pull", "appended 0 lines"} {
		if !strings.Contains(out.text, want) {
			t.Errorf("report lacks %q:\n%s", want, out.text)
		}
	}
	if v := callTool(t, cs, "verify", nil); v.isError || strings.Contains(v.text, "FAIL") {
		t.Fatalf("verify after the empty snapshot: %s", v.text)
	}
	plan := callTool(t, cs, "history_plan", map[string]any{"account": "paper"})
	if plan.isError || !strings.Contains(plan.text, "last observed 2026-11-10T15:35:00Z") {
		t.Errorf("history_plan after the first quiet pull:\n%s", plan.text)
	}

	// A second quiet pull is incremental (a window, not a full history) and
	// moves the observation forward.
	f.calls = nil
	*now = now.Add(30 * time.Minute)
	if out := callTool(t, cs, "fetch_snapshot", map[string]any{"book": "paper-wheel", "snapshot": "2026-11-10T1600"}); out.isError {
		t.Fatalf("second quiet fetch: %s", out.text)
	} else if strings.Contains(out.text, "full history pull") || !strings.Contains(strings.Join(f.calls, "\n"), "after=2026-11-09T00%3A00%3A00Z") {
		t.Errorf("second quiet pull wasn't incremental:\n%s\n%v", out.text, f.calls)
	}
	plan = callTool(t, cs, "history_plan", map[string]any{"account": "paper"})
	if !strings.Contains(plan.text, "last observed 2026-11-10T16:05:00Z") {
		t.Errorf("history_plan after the second quiet pull:\n%s", plan.text)
	}

	// The first orders arrive: they append from line 1, and the log still verifies.
	f.empty = false
	*now = now.Add(30 * time.Minute)
	out = callTool(t, cs, "fetch_snapshot", map[string]any{"book": "paper-wheel", "snapshot": "2026-11-10T1630"})
	if out.isError || !strings.Contains(out.text, "appended 5 lines (log now 5)") {
		t.Fatalf("first orders after an empty position: error=%v\n%s", out.isError, out.text)
	}
	if v := callTool(t, cs, "verify", nil); v.isError || strings.Contains(v.text, "FAIL") {
		t.Fatalf("verify after the first orders: %s", v.text)
	}
}
