// Package rhmcp is ob-mcp's client for Robinhood's MCP server, used only
// to fill a snapshot (CLAUDE.md's second, read-only exception; PLAN.md
// "Phase 5 design"). It can call nothing but the tools on Allowlist: the
// check runs before any network call, and init refuses to start if the list
// ever gains a name that could change something at the broker.
//
// What a call returns is the tool's text body, result.content[0].text,
// byte for byte: that is what the connector ingests, and it reproduced a
// connector-ingested body's sha256 exactly (spike/rhprobe, 11/17). Robinhood's
// "guide" field is kept.
package rhmcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// Allowlist is every Robinhood tool ob-mcp may call. Read-only, all of
// them; TestAllowlistIsReadOnly fails the build if a mutating name is added.
var Allowlist = []string{
	"get_accounts",
	"get_portfolio",
	"get_equity_positions",
	"get_option_positions",
	"get_option_orders",
	"get_equity_quotes",
	"get_option_quotes",
	"get_option_instruments",
	"get_option_chains",
	"get_realized_pnl",
	"get_pnl_trade_history",
	"get_equity_historicals",
}

// AdHoc are the further read tools rh_read may call (PLAN.md, addendum A): the
// same read-only check applies. fetch_snapshot and ticket use Allowlist only.
var AdHoc = []string{
	"get_earnings_calendar",
	"get_earnings_results",
	"get_equity_fundamentals",
	"get_index_quotes",
}

// MutatingPrefixes start the name of every Robinhood tool that can change
// something (rhprobe listed 36). None may be on Allowlist.
var MutatingPrefixes = []string{
	"place_", "cancel_", "exercise_", "review_", "preview_", "create_", "update_", "delete_",
	"add_", "remove_", "set_", "follow_", "unfollow_", "mark_", "decline_", "run_",
}

func init() {
	if err := CheckAllowlist(append(append([]string{}, Allowlist...), AdHoc...)); err != nil {
		panic(err)
	}
}

// CheckAllowlist refuses any name that is mutating or isn't a plain read.
func CheckAllowlist(names []string) error {
	for _, n := range names {
		for _, p := range MutatingPrefixes {
			if strings.HasPrefix(n, p) {
				return fmt.Errorf("rhmcp: %q is on the allowlist but can change something at the broker", n)
			}
		}
		if !strings.HasPrefix(n, "get_") {
			return fmt.Errorf("rhmcp: %q is on the allowlist but isn't a get_ tool", n)
		}
	}
	return nil
}

// Allowed reports whether a tool may be called at all: Allowlist or AdHoc.
func Allowed(name string) bool {
	return slices.Contains(Allowlist, name) || slices.Contains(AdHoc, name)
}

// Params lists the input names Robinhood's server advertises for a tool,
// once the session has started; nil if the tool isn't offered.
func (c *Client) Params(ctx context.Context, tool string) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.start(ctx); err != nil {
		return nil, err
	}
	var out []string
	for k := range c.schemas[tool] {
		out = append(out, k)
	}
	slices.Sort(out)
	return out, nil
}

// Authorizer adds the credential to a request (cred.Provider.Authorize).
type Authorizer func(ctx context.Context, req *http.Request) error

// Client is one session with Robinhood's MCP server.
type Client struct {
	Endpoint string
	HC       *http.Client
	Auth     Authorizer
	Now      func() time.Time // stamps FetchedAt; time.Now unless set

	mu      sync.Mutex
	session string
	id      int
	schemas map[string]map[string]any // tool → inputSchema.properties
}

func New(endpoint string, hc *http.Client, auth Authorizer) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	return &Client{Endpoint: endpoint, HC: hc, Auth: auth}
}

// Response is one stored-ready tool result: the body and the exact
// arguments it was fetched with.
type Response struct {
	Tool      string
	Args      map[string]any
	Body      []byte
	FetchedAt time.Time
}

// Call makes one tool call and returns its text body. Refused before any
// network call unless the tool is on Allowlist. An isError result is an
// error and nothing is returned to store.
func (c *Client) Call(ctx context.Context, tool string, args map[string]any) (*Response, error) {
	if !Allowed(tool) {
		return nil, fmt.Errorf("rhmcp: %s is not on the read-only allowlist; refused", tool)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.start(ctx); err != nil {
		return nil, err
	}
	if _, ok := c.schemas[tool]; !ok {
		return nil, fmt.Errorf("rhmcp: Robinhood's server doesn't offer %s", tool)
	}
	now := c.Now
	if now == nil {
		now = time.Now
	}
	at := now().UTC()
	raw, err := c.rpc(ctx, "tools/call", map[string]any{"name": tool, "arguments": args})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", tool, err)
	}
	var r struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("%s: unreadable response: %w", tool, err)
	}
	if len(r.Result.Content) == 0 || r.Result.Content[0].Type != "text" {
		return nil, fmt.Errorf("%s: no text content in the response", tool)
	}
	if r.Result.IsError {
		return nil, fmt.Errorf("%s: Robinhood returned an error: %s", tool, clip([]byte(r.Result.Content[0].Text)))
	}
	return &Response{Tool: tool, Args: args, Body: []byte(r.Result.Content[0].Text), FetchedAt: at}, nil
}

// cursorParams are the input names a paginated tool may take its cursor by.
var cursorParams = []string{"cursor", "next", "next_cursor", "page_cursor", "page_token"}

// Pages calls a tool and follows every next cursor, returning each page in
// order. If a page has a next cursor and the tool's input schema names no
// cursor parameter, that's an error: a partial history silently undercounts.
func (c *Client) Pages(ctx context.Context, tool string, args map[string]any) ([]*Response, error) {
	var out []*Response
	a := args
	for page := 1; ; page++ {
		if page > 100 {
			return nil, fmt.Errorf("%s: more than 100 pages; refusing to continue", tool)
		}
		r, err := c.Call(ctx, tool, a)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
		next, err := NextCursor(r.Body)
		if err != nil {
			return nil, fmt.Errorf("%s page %d: %w", tool, page, err)
		}
		if next == "" {
			return out, nil
		}
		param := ""
		c.mu.Lock()
		for _, p := range cursorParams {
			if _, ok := c.schemas[tool][p]; ok {
				param = p
				break
			}
		}
		c.mu.Unlock()
		if param == "" {
			return nil, fmt.Errorf("%s page %d has a next cursor, but the tool's input schema names no cursor parameter; can't fetch the rest", tool, page)
		}
		a = map[string]any{}
		for k, v := range args {
			a[k] = v
		}
		a[param] = next
	}
}

// NextCursor reads data.next from a body; "" when there is no further page.
func NextCursor(body []byte) (string, error) {
	var b struct {
		Data struct {
			Next *string `json:"next"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &b); err != nil {
		return "", fmt.Errorf("body isn't JSON: %w", err)
	}
	if b.Data.Next == nil {
		return "", nil
	}
	return *b.Data.Next, nil
}

// start opens the session once: initialize, initialized, tools/list.
func (c *Client) start(ctx context.Context) error {
	if c.schemas != nil {
		return nil
	}
	if _, err := c.rpc(ctx, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "ob-mcp", "version": "phase5"},
	}); err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	c.notify(ctx, "notifications/initialized")
	raw, err := c.rpc(ctx, "tools/list", map[string]any{})
	if err != nil {
		return fmt.Errorf("tools/list: %w", err)
	}
	var tl struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				InputSchema struct {
					Properties map[string]any `json:"properties"`
				} `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &tl); err != nil {
		return fmt.Errorf("tools/list: %w", err)
	}
	c.schemas = map[string]map[string]any{}
	for _, t := range tl.Result.Tools {
		if Allowed(t.Name) { // only the read tools are remembered at all
			c.schemas[t.Name] = t.InputSchema.Properties
		}
	}
	return nil
}

func (c *Client) post(ctx context.Context, payload map[string]any) (*http.Response, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	if err := c.Auth(ctx, req); err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2025-06-18")
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	return c.HC.Do(req)
}

func (c *Client) notify(ctx context.Context, method string) {
	if resp, err := c.post(ctx, map[string]any{"jsonrpc": "2.0", "method": method}); err == nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

// rpc sends one request and returns the JSON-RPC response message, whether
// the server answers with JSON or an event stream (rhprobe's reader).
func (c *Client) rpc(ctx context.Context, method string, params map[string]any) ([]byte, error) {
	c.id++
	id := c.id
	resp, err := c.post(ctx, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if s := resp.Header.Get("Mcp-Session-Id"); s != "" {
		c.session = s
	}
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, clip(b))
	}
	var msg []byte
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 64<<20)
		var data []string
		flush := func() {
			if len(data) == 0 || msg != nil {
				data = nil
				return
			}
			cand := []byte(strings.Join(data, "\n"))
			data = nil
			var probe struct {
				ID *int `json:"id"`
			}
			if json.Unmarshal(cand, &probe) == nil && probe.ID != nil && *probe.ID == id {
				msg = cand
			}
		}
		for msg == nil && sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "data:"):
				data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			case line == "":
				flush()
			}
		}
		flush()
		if msg == nil {
			return nil, fmt.Errorf("event stream ended without a response to request %d", id)
		}
	} else if msg, err = io.ReadAll(io.LimitReader(resp.Body, 64<<20)); err != nil {
		return nil, err
	}
	var e struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(msg, &e) == nil && e.Error != nil {
		return nil, fmt.Errorf("server error %d: %s", e.Error.Code, e.Error.Message)
	}
	return msg, nil
}

func clip(b []byte) string {
	if len(b) > 300 {
		return string(b[:300]) + "…"
	}
	return string(b)
}

// Property is one input's schema (its "type", say) as Robinhood's server
// advertises it; ok is false if the tool doesn't take that input.
func (c *Client) Property(ctx context.Context, tool, name string) (map[string]any, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.start(ctx); err != nil {
		return nil, false, err
	}
	p, ok := c.schemas[tool][name]
	if !ok {
		return nil, false, nil
	}
	m, _ := p.(map[string]any)
	return m, true, nil
}
