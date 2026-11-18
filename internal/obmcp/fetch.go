package obmcp

// fetch_snapshot and cred_status (PLAN.md, "Phase 5 design"): a check-in's
// snapshot pulled by the server straight from Robinhood's MCP server, through
// internal/rhmcp's read-only allowlist, and stored verbatim — no response is
// retyped. The server only reads the Robinhood credential; the
// wheel-cred-refresher job alone refreshes it.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/branonm/options-book/internal/layout"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/branonm/options-book/internal/broker"
	"github.com/branonm/options-book/internal/cred"
	"github.com/branonm/options-book/internal/history"
	"github.com/branonm/options-book/internal/report"
	"github.com/branonm/options-book/internal/rhmcp"
	"github.com/branonm/options-book/internal/snapshot"
	"github.com/branonm/options-book/internal/store"
)

// RobinhoodSource is the raw_response source of a server-side pull.
const RobinhoodSource = "robinhood/mcp-server"

func registerFetch(server *mcp.Server, s *tools) {
	w := &mcp.ToolAnnotations{DestructiveHint: boolp(false), IdempotentHint: false, OpenWorldHint: boolp(true)}
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: boolp(false)}
	mcp.AddTool(server, &mcp.Tool{Name: "fetch_snapshot", Annotations: w,
		Description: "Pull a whole snapshot straight from Robinhood and store it verbatim: open_snapshot, then per account every get_option_orders pull history_plan names, positions and portfolio, then quotes for held symbols and open contracts; history_add per account; seal. " +
			"Read-only at Robinhood (a compiled allowlist of get_ tools). On a partial failure the snapshot stays open and the error names what's missing; retry under a NEW id, since a re-pull differs. Reports the days left on the Robinhood access token, never the token."},
		expose(s.fetchSnapshot))
	mcp.AddTool(server, &mcp.Tool{Name: "cred_status", Annotations: ro,
		Description: "Each stored credential's secret version and, for Robinhood, when its access token expires and how many days are left. Never shows a token or key."},
		expose(s.credStatus))
	mcp.AddTool(server, &mcp.Tool{Name: "rh_read", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: boolp(true)},
		Description: "Read-only pass-through to Robinhood's MCP server for ad-hoc lookups: one call to a get_ tool on the compiled allowlist, or get_earnings_calendar, get_earnings_results, get_equity_fundamentals or get_index_quotes. The response is stored verbatim (no snapshot) and returned exactly as Robinhood sent it; its sha256 is in the result."},
		expose(s.rhRead))
}

func (s *tools) credStatus(ctx context.Context, _ *mcp.CallToolRequest, _ NoIn) (*mcp.CallToolResult, Result, error) {
	var b bytes.Buffer
	for _, p := range []cred.Provider{s.d.Robinhood, s.d.AlpacaPaper} {
		if p == nil {
			continue
		}
		st, err := cred.Describe(ctx, s.d.Vault, p, s.d.Now())
		switch {
		case cred.IsNotExist(err):
			fmt.Fprintf(&b, "%s: not enrolled (ob cred enroll %s)\n", p.ID(), p.ID())
		case err != nil:
			fmt.Fprintf(&b, "%s: %v\n", p.ID(), err)
		default:
			fmt.Fprintln(&b, st)
		}
	}
	return &mcp.CallToolResult{Content: text(&b)}, Result{}, nil
}

// robinhood opens a Robinhood MCP client with the latest stored token, and
// reports the credential's status (version, days left; never the token).
func (s *tools) robinhood(ctx context.Context) (*rhmcp.Client, *cred.Status, error) {
	if s.d.Vault == nil || s.d.Robinhood == nil {
		return nil, nil, errors.New("this server has no Robinhood credential configured")
	}
	st, err := cred.Describe(ctx, s.d.Vault, s.d.Robinhood, s.d.Now())
	if err != nil {
		return nil, nil, fmt.Errorf("reading the Robinhood credential: %w", err)
	}
	secret, err := s.d.Vault.Latest(ctx, "robinhood-oauth")
	if err != nil {
		return nil, nil, err
	}
	rc := rhmcp.New(s.d.RobinhoodMCP, s.d.RobinhoodHTTP, func(ctx context.Context, req *http.Request) error {
		return s.d.Robinhood.Authorize(ctx, secret.Data, req)
	})
	rc.Now = s.d.Now
	return rc, st, nil
}

type RHReadIn struct {
	Tool string         `json:"tool" jsonschema:"a read-only Robinhood MCP tool, e.g. get_equity_fundamentals"`
	Args map[string]any `json:"args,omitempty" jsonschema:"its arguments, exactly as Robinhood's tool takes them"`
}

// rhRead is a read-only pass-through for ad-hoc lookups: any tool on the
// allowlist or AdHoc, one call, the body stored by sha256 (no snapshot) and
// returned verbatim.
func (s *tools) rhRead(ctx context.Context, _ *mcp.CallToolRequest, in RHReadIn) (*mcp.CallToolResult, Result, error) {
	if !rhmcp.Allowed(in.Tool) {
		return nil, Result{}, fmt.Errorf("%s is not a read-only tool rh_read may call (allowed: %s)", in.Tool, strings.Join(append(append([]string{}, rhmcp.Allowlist...), rhmcp.AdHoc...), ", "))
	}
	rc, _, err := s.robinhood(ctx)
	if err != nil {
		return nil, Result{}, err
	}
	if in.Args == nil {
		in.Args = map[string]any{}
	}
	r, err := rc.Call(ctx, in.Tool, in.Args)
	if err != nil {
		return nil, Result{}, err
	}
	args, _ := json.Marshal(r.Args)
	sha, _, err := store.StoreRaw(ctx, s.d.DB, store.Raw{Call: r.Tool, Args: string(args), Body: r.Body, FetchedAt: r.FetchedAt, Source: RobinhoodSource})
	if err != nil {
		return nil, Result{}, err
	}
	return &mcp.CallToolResult{Content: text(bytes.NewBuffer(r.Body))}, Result{SHA256: sha}, nil
}

type FetchIn struct {
	Book     string `json:"book,omitempty" jsonschema:"the book whose accounts to pull; default ira-wheel"`
	Snapshot string `json:"snapshot" jsonschema:"the new snapshot's id, e.g. 2026-11-18 or 2026-11-18T1032; never one used before"`
	Note     string `json:"note,omitempty" jsonschema:"what this snapshot is for"`
}

// fetchErr is a failure after the snapshot was opened: the progress so far,
// what is missing, and that a retry needs a new id.
func fetchErr(log *bytes.Buffer, id string, opened bool, err error) error {
	if !opened {
		return fmt.Errorf("%snothing was stored: %w", log.String(), err)
	}
	return fmt.Errorf("%s%w\nSnapshot %s is left open and unsealed with what is listed above. A retry must use a NEW id: a re-pull differs, and a stored response never changes", log.String(), err, id)
}

func (s *tools) fetchSnapshot(ctx context.Context, _ *mcp.CallToolRequest, in FetchIn) (*mcp.CallToolResult, Result, error) {
	var log bytes.Buffer
	bc, err := s.bk(in.Book)
	if err != nil {
		return nil, Result{}, err
	}
	if bc.Cfg.Source() == alpacaBroker {
		return s.fetchSnapshotAlpaca(ctx, in, bc, &log)
	}
	if s.d.Vault == nil || s.d.Robinhood == nil {
		return nil, Result{}, errors.New("this server has no Robinhood credential configured")
	}
	rc, st, err := s.robinhood(ctx)
	if err != nil {
		return nil, Result{}, err
	}
	fmt.Fprintf(&log, "Robinhood credential: %s\n", st)
	fmt.Fprintf(&log, "Book %s: pulling its %d accounts.\n", bc.Name, len(bc.Cfg.Accounts))
	dec, err := snapshot.Decoder()
	if err != nil {
		return nil, Result{}, err
	}
	f := &fetcher{t: s, rc: rc, dec: dec, bc: bc, id: in.Snapshot, log: &log, pulls: map[string][][]string{}}

	// Before anything is stored: the token works and the accounts resolve.
	accts, err := f.accounts(ctx)
	if err != nil {
		return nil, Result{}, fetchErr(&log, in.Snapshot, false, err)
	}
	note := in.Note
	if note == "" {
		note = "pulled by fetch_snapshot"
	}
	if err := store.OpenSnapshot(ctx, s.d.DB, in.Snapshot, note, s.d.Now()); err != nil {
		return nil, Result{}, fetchErr(&log, in.Snapshot, false, err)
	}
	s.invalidate()
	fmt.Fprintf(&log, "Opened snapshot %s.\n", in.Snapshot)
	if err := f.fill(ctx, accts); err != nil {
		s.invalidate()
		return nil, Result{}, fetchErr(&log, in.Snapshot, true, err)
	}
	s.invalidate()
	src, err := s.source(ctx)
	if err != nil {
		return nil, Result{}, fetchErr(&log, in.Snapshot, true, err)
	}
	ps, sealRes, err := store.Seal(ctx, s.d.DB, src, in.Snapshot, report.AccountRefs(bc.Cfg), bc.Cfg.Source(), s.d.Now())
	s.invalidate()
	if err != nil {
		return nil, Result{}, fetchErr(&log, in.Snapshot, true, err)
	}
	if len(ps) > 0 {
		var pb bytes.Buffer
		report.SnapshotProblems(&pb, layout.Snapshot(in.Snapshot), ps)
		return nil, Result{}, fetchErr(&log, in.Snapshot, true, fmt.Errorf("the snapshot isn't complete, so it wasn't sealed:\n%s", pb.String()))
	}
	fmt.Fprintf(&log, "Sealed snapshot %s (%d members and positions). Every response above is stored verbatim from Robinhood's MCP server.\n", in.Snapshot, sealRes.Members)
	fmt.Fprintf(&log, "Robinhood access token: %.1f days left.\n", *st.DaysLeft)
	return &mcp.CallToolResult{Content: text(&log)}, Result{}, nil
}

type fetcher struct {
	t     *tools
	rc    *rhmcp.Client
	dec   broker.Decoder
	bc    *BookConfig
	id    string
	log   *bytes.Buffer
	pulls map[string][][]string // account → page names per query
	prov  []string              // PROVENANCE.md table rows
}

type account struct {
	name, number string
}

func mask(number string) string {
	if len(number) <= 4 {
		return "••" + number
	}
	return "••" + number[len(number)-4:]
}

// accounts maps each configured account to its Robinhood account number by
// the last four digits. Exactly one match per account, or an error.
func (f *fetcher) accounts(ctx context.Context) ([]account, error) {
	r, err := f.rc.Call(ctx, "get_accounts", map[string]any{})
	if err != nil {
		return nil, err
	}
	var v any
	if err := json.Unmarshal(r.Body, &v); err != nil {
		return nil, fmt.Errorf("get_accounts: %w", err)
	}
	var numbers []string
	var walk func(any)
	walk = func(x any) {
		switch x := x.(type) {
		case map[string]any:
			for k, val := range x {
				if s, ok := val.(string); ok && k == "account_number" {
					numbers = append(numbers, s)
				}
				walk(val)
			}
		case []any:
			for _, val := range x {
				walk(val)
			}
		}
	}
	walk(v)
	var out []account
	for _, a := range f.bc.Cfg.Accounts {
		var hits []string
		for _, n := range numbers {
			if strings.HasSuffix(n, a.Last4) {
				hits = append(hits, n)
			}
		}
		if len(uniq(hits)) != 1 {
			return nil, fmt.Errorf("get_accounts: %d accounts end in %s (%s); want exactly one", len(uniq(hits)), a.Last4, a.Name)
		}
		out = append(out, account{name: a.Name, number: hits[0]})
	}
	fmt.Fprintf(f.log, "Accounts: %s.\n", strings.Join(func() []string {
		var s []string
		for _, a := range out {
			s = append(s, a.name+" "+mask(a.number))
		}
		return s
	}(), ", "))
	return out, nil
}

func uniq(s []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range s {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// put stores one response verbatim.
func (f *fetcher) put(ctx context.Context, r *rhmcp.Response, role, acct, key string) error {
	args, err := json.Marshal(r.Args)
	if err != nil {
		return err
	}
	res, err := store.IngestResponse(ctx, f.t.d.DB, store.Ingest{
		Snapshot: f.id, Role: role, Account: acct, Instrument: key, Call: r.Tool, Args: string(args),
		Body: r.Body, Verbatim: true, FetchedAt: r.FetchedAt.Format(time.RFC3339Nano), Source: RobinhoodSource,
	}, f.t.d.Now())
	if err != nil {
		return fmt.Errorf("storing %s: %w", r.Tool, err)
	}
	shown := map[string]any{}
	for k, v := range r.Args {
		if k == "account_number" {
			v = mask(fmt.Sprint(v))
		}
		shown[k] = v
	}
	sa, _ := json.Marshal(shown)
	rel := strings.TrimPrefix(res.Path, layout.Snapshot(f.id)+"/")
	f.prov = append(f.prov, fmt.Sprintf("| `%s` | `%s(%s)` | %s | `%s…` |", rel, r.Tool, sa, r.FetchedAt.Format(time.RFC3339), res.SHA256[:12]))
	fmt.Fprintf(f.log, "  %-38s %s %s, %d bytes\n", rel, r.Tool, sa, res.Bytes)
	return nil
}

// one is a call that must fit on one page.
func (f *fetcher) one(ctx context.Context, tool string, args map[string]any) (*rhmcp.Response, error) {
	pages, err := f.rc.Pages(ctx, tool, args)
	if err != nil {
		return nil, err
	}
	if len(pages) != 1 {
		return nil, fmt.Errorf("%s came back in %d pages; the store holds this response as one body", tool, len(pages))
	}
	return pages[0], nil
}

func (f *fetcher) fill(ctx context.Context, accts []account) error {
	src, err := f.t.source(ctx)
	if err != nil {
		return err
	}
	symbols, options := map[string]bool{}, map[string]bool{}
	observed := map[string]time.Time{}
	since := map[string]string{}
	for _, a := range accts {
		l, err := history.LoadFS(src.FS, store.LogPath(a.name))
		if err != nil {
			return err
		}
		if len(l.Entries) == 0 {
			return fmt.Errorf("the %s order log is empty: its first, full pull is done by hand (ob history add --full)", a.name)
		}
		plan := l.NextPlan()
		since[a.name] = plan.CreatedSince
		fmt.Fprintf(f.log, "%s %s (log %d lines; history_plan: created since %s, %d open in the log):\n", a.name, mask(a.number), len(l.Entries), plan.CreatedSince, len(plan.RefreshIDs))

		seen := map[string]bool{}
		query := func(prefix string, args map[string]any) error {
			pages, err := f.rc.Pages(ctx, "get_option_orders", args)
			if err != nil {
				return err
			}
			var names []string
			for i, p := range pages {
				name := fmt.Sprintf("%s_p%d", prefix, i+1)
				if err := f.put(ctx, p, store.RoleOrdersPull, a.name, name); err != nil {
					return err
				}
				names = append(names, name)
				if i == 0 && prefix == "window" {
					observed[a.name] = p.FetchedAt.Truncate(time.Second)
				}
				records, _, err := history.PageRecords(name, p.Body)
				if err != nil {
					return err
				}
				for _, raw := range records {
					h, err := history.OrderHead(raw)
					if err != nil {
						return err
					}
					seen[h.ID] = true
				}
			}
			f.pulls[a.name] = append(f.pulls[a.name], names)
			return nil
		}
		if err := query("window", map[string]any{"account_number": a.number, "created_at_gte": plan.CreatedSince}); err != nil {
			return err
		}
		if err := query("open", map[string]any{"account_number": a.number, "state": "confirmed"}); err != nil {
			return err
		}
		for _, id := range plan.RefreshIDs {
			if seen[id] {
				continue
			}
			r, err := f.one(ctx, "get_option_orders", map[string]any{"account_number": a.number, "order_id": id})
			if err != nil {
				return err
			}
			name := "lookup_" + id[:min(8, len(id))]
			if err := f.put(ctx, r, store.RoleOrdersPull, a.name, name); err != nil {
				return err
			}
			f.pulls[a.name] = append(f.pulls[a.name], []string{name})
		}

		r, err := f.one(ctx, "get_equity_positions", map[string]any{"account_number": a.number})
		if err != nil {
			return err
		}
		if err := f.put(ctx, r, store.RoleEquityPositions, a.name, ""); err != nil {
			return err
		}
		eqs, err := f.dec.EquityPositions("get_equity_positions", r.Body)
		if err != nil {
			return err
		}
		for _, p := range eqs {
			symbols[p.Symbol] = true
		}
		if r, err = f.one(ctx, "get_option_positions", map[string]any{"account_number": a.number, "nonzero": true}); err != nil {
			return err
		}
		if err := f.put(ctx, r, store.RoleOptionPositions, a.name, ""); err != nil {
			return err
		}
		ops, err := f.dec.OptionPositions("get_option_positions", r.Body)
		if err != nil {
			return err
		}
		for _, p := range ops {
			options[p.InstrumentID] = true
		}
		if r, err = f.one(ctx, "get_portfolio", map[string]any{"account_number": a.number}); err != nil {
			return err
		}
		if err := f.put(ctx, r, store.RolePortfolio, a.name, ""); err != nil {
			return err
		}
	}

	fmt.Fprintln(f.log, "Quotes:")
	if len(symbols) > 0 {
		r, err := f.one(ctx, "get_equity_quotes", map[string]any{"symbols": sorted(symbols)})
		if err != nil {
			return err
		}
		if err := f.put(ctx, r, store.RoleEquityQuotes, "", ""); err != nil {
			return err
		}
	}
	if len(options) > 0 {
		r, err := f.one(ctx, "get_option_quotes", map[string]any{"instrument_ids": sorted(options)})
		if err != nil {
			return err
		}
		if err := f.put(ctx, r, store.RoleOptionQuotes, "", ""); err != nil {
			return err
		}
	}

	f.t.invalidate()
	src, err = f.t.source(ctx)
	if err != nil {
		return err
	}
	for _, a := range accts {
		cs, _ := time.Parse("2006-01-02", since[a.name])
		res, err := store.AddHistory(ctx, f.t.d.DB, src, store.HistoryAdd{
			Snapshot: f.id, Account: a.name, ObservedAt: observed[a.name], CreatedSince: cs, Pulls: f.pulls[a.name],
		})
		if err != nil {
			return fmt.Errorf("history_add %s: %w", a.name, err)
		}
		f.t.invalidate()
		if src, err = f.t.source(ctx); err != nil {
			return err
		}
		fmt.Fprintf(f.log, "%s: %d new orders, %d changed, %d unchanged; appended %d lines (log now %d).\n",
			a.name, res.NewOrders, res.Changed, res.Unchanged, res.Appended, res.Lines)
	}

	prov := fmt.Sprintf("# Snapshot %s — provenance\n\nPulled by `fetch_snapshot` (ob-mcp %s) directly from Robinhood's MCP server through the read-only allowlist. "+
		"Every response is stored **verbatim**: the tool's `result.content[0].text`, byte for byte. Account numbers are masked here and stored in full in each response's args.\n\n"+
		"| File | Call | Fetched (UTC) | sha256 |\n|---|---|---|---|\n%s\n", f.id, f.t.d.Commit, strings.Join(f.prov, "\n"))
	if _, err := store.IngestResponse(ctx, f.t.d.DB, store.Ingest{
		Snapshot: f.id, Role: store.RoleProvenance, Body: []byte(prov), Verbatim: true,
	}, f.t.d.Now()); err != nil {
		return fmt.Errorf("storing PROVENANCE.md: %w", err)
	}
	return nil
}

func sorted(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
