package rhmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestAllowlistIsReadOnly is the build-failing guard (CLAUDE.md, PLAN.md
// phase 5): no allowlisted name may start with a mutating prefix, and every
// one is a get_ tool. Adding place_option_order, say, fails go test ./...
func TestAllowlistIsReadOnly(t *testing.T) {
	if err := CheckAllowlist(append(append([]string{}, Allowlist...), AdHoc...)); err != nil {
		t.Fatal(err)
	}
	if len(Allowlist) != 12 || len(AdHoc) != 4 {
		t.Errorf("%d + %d tools; PLAN.md names 12 for fetch_snapshot and 4 more for rh_read", len(Allowlist), len(AdHoc))
	}
	for _, bad := range []string{"place_option_order", "cancel_equity_order", "review_option_order", "preview_crypto_order",
		"exercise_option", "add_to_watchlist", "run_scan", "update_alert", "decline_trade_approval", "mark_alerts_read", "search"} {
		if CheckAllowlist([]string{bad}) == nil {
			t.Errorf("CheckAllowlist accepted %s", bad)
		}
	}
}

// fakeServer is Robinhood's MCP server: read tools, the mutating ones it
// also offers, pagination on get_option_orders, an isError on demand.
type fakeServer struct {
	t       *testing.T
	mu      sync.Mutex
	calls   []string // tools/call names received
	sse     bool
	cursor  string // the input name get_option_orders takes its cursor by; "" = none
	session string
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer good" {
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
		w.Header().Set("Mcp-Session-Id", "sess-1")
		result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}}
	case "tools/list":
		if r.Header.Get("Mcp-Session-Id") != "sess-1" {
			w.WriteHeader(400)
			return
		}
		orderProps := map[string]any{"account_number": map[string]any{}, "created_at_gte": map[string]any{}, "state": map[string]any{}}
		if f.cursor != "" {
			orderProps[f.cursor] = map[string]any{}
		}
		tools := []any{
			map[string]any{"name": "get_option_orders", "inputSchema": map[string]any{"properties": orderProps}},
			map[string]any{"name": "get_portfolio", "inputSchema": map[string]any{"properties": map[string]any{"account_number": map[string]any{}}}},
			map[string]any{"name": "place_option_order", "inputSchema": map[string]any{}},
			map[string]any{"name": "cancel_option_order", "inputSchema": map[string]any{}},
		}
		result = map[string]any{"tools": tools}
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		json.Unmarshal(req.Params, &p)
		f.calls = append(f.calls, p.Name)
		text, isErr := "", false
		switch p.Name {
		case "get_portfolio":
			if p.Arguments["account_number"] == "bad" {
				text, isErr = "account not found", true
			} else {
				text = `{"data":{"total_value":"1000.00"},"guide":"Values are in USD."}`
			}
		case "get_option_orders":
			next := "null"
			if f.cursor == "" || p.Arguments[f.cursor] == nil {
				next = `"c2"`
			}
			text = fmt.Sprintf(`{"data":{"orders":[{"id":"o-%v"}],"next":%s}}`, p.Arguments[f.cursor], next)
		default:
			text, isErr = "no such tool", true
		}
		result = map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}, "isError": isErr}
	}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": result})
	if f.sse {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

func client(t *testing.T, f *fakeServer) *Client {
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return New(srv.URL, srv.Client(), func(_ context.Context, r *http.Request) error {
		r.Header.Set("Authorization", "Bearer good")
		return nil
	})
}

func TestCallReturnsTheTextBody(t *testing.T) {
	for _, sse := range []bool{false, true} {
		f := &fakeServer{t: t, sse: sse}
		c := client(t, f)
		r, err := c.Call(context.Background(), "get_portfolio", map[string]any{"account_number": "123"})
		if err != nil {
			t.Fatalf("sse=%v: %v", sse, err)
		}
		// content[0].text byte for byte, guide included — not the JSON-RPC envelope.
		if string(r.Body) != `{"data":{"total_value":"1000.00"},"guide":"Values are in USD."}` {
			t.Errorf("sse=%v: body %s", sse, r.Body)
		}
		if r.Args["account_number"] != "123" || r.Tool != "get_portfolio" || r.FetchedAt.IsZero() {
			t.Errorf("sse=%v: %+v", sse, r)
		}
	}
}

func TestRefusals(t *testing.T) {
	f := &fakeServer{t: t}
	c := client(t, f)
	ctx := context.Background()
	// Not on the allowlist: refused before any request, even though the server offers it.
	for _, tool := range []string{"place_option_order", "cancel_option_order", "search"} {
		if _, err := c.Call(ctx, tool, nil); err == nil || !strings.Contains(err.Error(), "allowlist") {
			t.Errorf("%s: %v", tool, err)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("refused tools reached the server: %v", f.calls)
	}
	// isError is an error, with nothing to store.
	if r, err := c.Call(ctx, "get_portfolio", map[string]any{"account_number": "bad"}); err == nil || r != nil || !strings.Contains(err.Error(), "account not found") {
		t.Errorf("isError: %v %v", r, err)
	}
	// Allowed, but not offered by the server.
	if _, err := c.Call(ctx, "get_realized_pnl", nil); err == nil || !strings.Contains(err.Error(), "doesn't offer") {
		t.Errorf("unoffered tool: %v", err)
	}
	// A wrong credential is an HTTP error.
	bad := New(c.Endpoint, c.HC, func(_ context.Context, r *http.Request) error { return nil })
	if _, err := bad.Call(ctx, "get_portfolio", nil); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("no token: %v", err)
	}
}

func TestPagesFollowNext(t *testing.T) {
	f := &fakeServer{t: t, cursor: "cursor"}
	c := client(t, f)
	pages, err := c.Pages(context.Background(), "get_option_orders", map[string]any{"account_number": "1", "created_at_gte": "2026-11-16"})
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 2 || pages[1].Args["cursor"] != "c2" || pages[1].Args["created_at_gte"] != "2026-11-16" || pages[0].Args["cursor"] != nil {
		t.Errorf("pages: %d, args %v / %v", len(pages), pages[0].Args, pages[len(pages)-1].Args)
	}
	// No cursor parameter in the schema: a refusal, never a silent first page.
	f2 := &fakeServer{t: t}
	if _, err := client(t, f2).Pages(context.Background(), "get_option_orders", map[string]any{"account_number": "1"}); err == nil || !strings.Contains(err.Error(), "no cursor parameter") {
		t.Errorf("unpageable: %v", err)
	}
}
