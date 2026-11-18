package obmcp

// The write tools (the storage design phase 4): a check-in done through the
// server. Every write is a create; nothing here can change or remove what is
// stored, and none of it reaches a broker. The order is the check-in
// procedure's: open_snapshot → ingest each response (orders_pull pages first)
// → history_add per account → ingest the rest → seal_snapshot; then the read
// tools; record_expiration when checkin reports an unresolved expiration.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/branonm/options-book/internal/layout"
	"io/fs"
	"slices"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/branonm/options-book/internal/broker"
	"github.com/branonm/options-book/internal/history"
	"github.com/branonm/options-book/internal/report"
	"github.com/branonm/options-book/internal/snapshot"
	"github.com/branonm/options-book/internal/store"
)

func registerWrites(server *mcp.Server, s *tools) {
	w := &mcp.ToolAnnotations{DestructiveHint: boolp(false), IdempotentHint: false, OpenWorldHint: boolp(false)}
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: boolp(false)}

	mcp.AddTool(server, &mcp.Tool{Name: "history_plan", Annotations: ro,
		Description: "Which get_option_orders pulls the next history_add needs for an account: the created-since window, the open orders, and every order open in the log that must be accounted for. Same output as `ob history plan`."},
		expose(s.historyPlan))
	mcp.AddTool(server, &mcp.Tool{Name: "open_snapshot", Annotations: w,
		Description: "Start a snapshot. The id dates everything computed from it: YYYY-MM-DD for a pre-market, after-close or weekend pull, YYYY-MM-DDTHHMM (exchange time) for an intraday one."},
		expose(s.openSnapshot))
	mcp.AddTool(server, &mcp.Tool{Name: "ingest", Annotations: w,
		Description: "Store one raw Robinhood response in a snapshot, exactly as returned. Roles: equity_positions, option_positions, portfolio, realized_pnl (per account); equity_quotes, option_quotes; option_quote and option_instrument (with instrument); orders_pull (per account, with page, e.g. window_p1, open_p1, lookup_6ac662e8); bars (with name, for record_expiration); provenance (the PROVENANCE.md text); expected. " +
			"The body is validated against the role's schema and refused if malformed. If you retyped it rather than copied it, set verbatim=false and say how in note. Stored responses never change: re-ingesting identical bytes is a no-op, different bytes are refused. A sealed snapshot takes only provenance, expected, realized_pnl and bars."},
		expose(s.ingest))
	mcp.AddTool(server, &mcp.Tool{Name: "history_add", Annotations: w,
		Description: "Append an account's ingested orders_pull pages to its order log and record the snapshot's position in it, with the same refusals as `ob history add`: a created-since window that leaves a gap, an observation not after the log's last, an open order missing from the pull. Refused on a sealed snapshot before anything is appended."},
		expose(s.historyAdd))
	mcp.AddTool(server, &mcp.Tool{Name: "record_expiration", Annotations: w,
		Description: "Record how contracts that ran through expiration with no closing order ended (assigned or expired worthless) from the underlying's regular-session close. Without bars, lists what to record and the get_equity_historicals call to make; ingest that response with role bars, then call again naming it. Same output as `ob expirations`."},
		expose(s.recordExpiration))
	mcp.AddTool(server, &mcp.Tool{Name: "seal_snapshot", Annotations: w,
		Description: "Check a snapshot is complete and consistent, exactly as `ob snapshot` does, and seal it: after that nothing more can be added to it. exit_code 1 lists what is missing, with the call that fixes each."},
		expose(s.sealSnapshot))
}

// invalidate drops the cached store after a write, so the next read sees it.
func (s *tools) invalidate() {
	s.mu.Lock()
	s.src = nil
	s.mu.Unlock()
}

func (s *tools) account(name string) error {
	if name == "" {
		return nil
	}
	if _, ok := s.d.Accounts.Account(name); ok {
		return nil
	}
	return fmt.Errorf("no account %q in %s", name, layout.AccountsConfig)
}

// brokerOf is the adapter a response or an order log is from: the account's own
// source when an account is named, else the optional explicit source, else
// Robinhood ("").
func (s *tools) brokerOf(account, explicit string) (string, error) {
	src := explicit
	if account != "" {
		if a, ok := s.d.Accounts.Account(account); ok {
			if explicit != "" && explicit != a.Source {
				return "", fmt.Errorf("account %s is read through %s, not %s", account, a.Source, explicit)
			}
			src = a.Source
		}
	}
	if src != "" {
		if _, err := broker.Lookup(src); err != nil {
			return "", err
		}
	}
	return src, nil
}

type HistoryPlanIn struct {
	Account string `json:"account" jsonschema:"account name, e.g. roth"`
}

func (s *tools) historyPlan(ctx context.Context, _ *mcp.CallToolRequest, in HistoryPlanIn) (*mcp.CallToolResult, Result, error) {
	if in.Account == "" {
		return nil, Result{}, fmt.Errorf("account is required")
	}
	if err := s.account(in.Account); err != nil {
		return nil, Result{}, err
	}
	src, err := s.source(ctx)
	if err != nil {
		return nil, Result{}, err
	}
	br, err := s.brokerOf(in.Account, "")
	if err != nil {
		return nil, Result{}, err
	}
	l, err := history.LoadFSFor(src.FS, store.LogPath(in.Account), br)
	if err != nil {
		return nil, Result{}, err
	}
	var b bytes.Buffer
	report.HistoryPlan(&b, in.Account, l, fmt.Sprintf("Then: ingest each page (role orders_pull, account %s), and history_add(account=%s, observed_at=<when>, created_since=%s, pulls=[[window pages], [open pages], [lookup], …]).", in.Account, in.Account, l.NextPlan().CreatedSince))
	return &mcp.CallToolResult{Content: text(&b)}, Result{}, nil
}

type OpenSnapshotIn struct {
	Snapshot string `json:"snapshot" jsonschema:"the new snapshot's id, e.g. 2026-11-17 or 2026-11-17T1032"`
	Note     string `json:"note,omitempty" jsonschema:"what this snapshot is for"`
}

func (s *tools) openSnapshot(ctx context.Context, _ *mcp.CallToolRequest, in OpenSnapshotIn) (*mcp.CallToolResult, Result, error) {
	if err := store.OpenSnapshot(ctx, s.d.DB, in.Snapshot, in.Note, s.d.Now()); err != nil {
		return nil, Result{}, err
	}
	s.invalidate()
	return &mcp.CallToolResult{Content: text(bytes.NewBufferString(fmt.Sprintf("Opened snapshot %s.\n", in.Snapshot)))}, Result{}, nil
}

type IngestIn struct {
	Snapshot   string         `json:"snapshot" jsonschema:"snapshot id"`
	Role       string         `json:"role" jsonschema:"what the response is; see the tool description"`
	Account    string         `json:"account,omitempty" jsonschema:"account name for per-account roles, e.g. roth"`
	Instrument string         `json:"instrument,omitempty" jsonschema:"option instrument id, for option_quote and option_instrument"`
	Page       string         `json:"page,omitempty" jsonschema:"orders_pull page name, e.g. window_p1, open_p1, lookup_6ac662e8"`
	Name       string         `json:"name,omitempty" jsonschema:"bars file name, e.g. 2026-11-21_kre-slv"`
	Call       string         `json:"call,omitempty" jsonschema:"the Robinhood MCP tool that returned it, e.g. get_portfolio"`
	Args       map[string]any `json:"args,omitempty" jsonschema:"the arguments that call was made with"`
	Body       string         `json:"body" jsonschema:"the response, exactly as returned"`
	Verbatim   bool           `json:"verbatim" jsonschema:"true only if body is the response copied byte for byte; false if retyped"`
	Note       string         `json:"note,omitempty" jsonschema:"required when not verbatim: how it was produced and checked"`
	FetchedAt  string         `json:"fetched_at,omitempty" jsonschema:"when the call was made, RFC3339"`
	Source     string         `json:"source,omitempty" jsonschema:"the broker adapter the response is from, for a response with no account (e.g. alpaca); default robinhood"`
}

func (s *tools) ingest(ctx context.Context, _ *mcp.CallToolRequest, in IngestIn) (*mcp.CallToolResult, Result, error) {
	if err := s.account(in.Account); err != nil {
		return nil, Result{}, err
	}
	key, n := "", 0
	for _, v := range []string{in.Instrument, in.Page, in.Name} {
		if v != "" {
			key, n = v, n+1
		}
	}
	if n > 1 {
		return nil, Result{}, fmt.Errorf("give at most one of instrument, page and name")
	}
	args := ""
	if in.Args != nil {
		b, err := json.Marshal(in.Args)
		if err != nil {
			return nil, Result{}, err
		}
		args = string(b)
	}
	br, err := s.brokerOf(in.Account, in.Source)
	if err != nil {
		return nil, Result{}, err
	}
	res, err := store.IngestResponse(ctx, s.d.DB, store.Ingest{
		Snapshot: in.Snapshot, Role: in.Role, Account: in.Account, Instrument: key, Call: in.Call, Args: args,
		Body: []byte(in.Body), Verbatim: in.Verbatim, Note: in.Note, FetchedAt: in.FetchedAt, Broker: br,
	}, s.d.Now())
	if err != nil {
		return nil, Result{}, err
	}
	s.invalidate()
	msg := fmt.Sprintf("Stored %s (%d bytes, sha256 %s).\n", res.Path, res.Bytes, res.SHA256)
	if !res.NewBody {
		msg = fmt.Sprintf("Stored %s (%d bytes, sha256 %s); identical bytes were already stored, so only the attachment is new.\n", res.Path, res.Bytes, res.SHA256)
	}
	return &mcp.CallToolResult{Content: text(bytes.NewBufferString(msg))}, Result{}, nil
}

type HistoryAddIn struct {
	Snapshot     string     `json:"snapshot" jsonschema:"snapshot id the pull belongs to"`
	Account      string     `json:"account" jsonschema:"account name, e.g. roth"`
	ObservedAt   string     `json:"observed_at" jsonschema:"when the pull was made, RFC3339, e.g. 2026-11-17T14:46:40Z"`
	CreatedSince string     `json:"created_since,omitempty" jsonschema:"the created_at_gte the window query used, YYYY-MM-DD (from history_plan)"`
	Full         bool       `json:"full,omitempty" jsonschema:"the pull is a full history, no created_at filter"`
	Pulls        [][]string `json:"pulls" jsonschema:"ingested page names, one list per query in page order; each single-order lookup is its own list"`
}

func (s *tools) historyAdd(ctx context.Context, _ *mcp.CallToolRequest, in HistoryAddIn) (*mcp.CallToolResult, Result, error) {
	if in.Account == "" {
		return nil, Result{}, fmt.Errorf("account is required")
	}
	if err := s.account(in.Account); err != nil {
		return nil, Result{}, err
	}
	obs, err := time.Parse(time.RFC3339, in.ObservedAt)
	if err != nil {
		return nil, Result{}, fmt.Errorf("observed_at: %w", err)
	}
	br, err := s.brokerOf(in.Account, "")
	if err != nil {
		return nil, Result{}, err
	}
	add := store.HistoryAdd{Snapshot: in.Snapshot, Account: in.Account, ObservedAt: obs, Full: in.Full, Pulls: in.Pulls, Broker: br}
	if in.CreatedSince != "" {
		if add.CreatedSince, err = time.Parse("2006-01-02", in.CreatedSince); err != nil {
			return nil, Result{}, fmt.Errorf("created_since: %w", err)
		}
	}
	// Read the latest log and pages, not a cached view.
	s.invalidate()
	src, err := s.source(ctx)
	if err != nil {
		return nil, Result{}, err
	}
	res, err := store.AddHistory(ctx, s.d.DB, src, add)
	s.invalidate()
	if err != nil {
		return nil, Result{}, err
	}
	msg := fmt.Sprintf("%s: %d new orders, %d changed, %d unchanged; appended %d lines (log now %d).\nRecorded snapshots/%s/%s/history.json (replays the first %d lines).\n",
		in.Account, res.NewOrders, res.Changed, res.Unchanged, res.Appended, res.Lines, in.Snapshot, in.Account, res.Lines)
	return &mcp.CallToolResult{Content: text(bytes.NewBufferString(msg))}, Result{}, nil
}

type RecordExpirationIn struct {
	Book     string `json:"book,omitempty" jsonschema:"the book to run, e.g. ira-wheel; default ira-wheel"`
	Snapshot string `json:"snapshot" jsonschema:"snapshot whose check-in reported expiration_unresolved"`
	Bars     string `json:"bars,omitempty" jsonschema:"name of the ingested bars response; omit to list what to fetch"`
}

func (s *tools) recordExpiration(ctx context.Context, _ *mcp.CallToolRequest, in RecordExpirationIn) (*mcp.CallToolResult, Result, error) {
	bc, err := s.bk(in.Book)
	if err != nil {
		return nil, Result{}, err
	}
	s.invalidate()
	src, err := s.source(ctx)
	if err != nil {
		return nil, Result{}, err
	}
	at, asOf, err := s.dated(src, in.Snapshot, "")
	if err != nil {
		return nil, Result{}, err
	}
	_, bk, err := report.Assemble(bc.Cfg, at, asOf)
	if err != nil {
		return nil, Result{}, err
	}
	var b bytes.Buffer
	todo := report.Unresolved(bk)
	if len(todo) == 0 {
		b.WriteString(report.NothingUnresolved)
		return &mcp.CallToolResult{Content: text(&b)}, Result{}, nil
	}
	if in.Bars == "" {
		report.ExpirationPlan(&b, todo, fmt.Sprintf("Ingest it into snapshot %s with role bars and a name, then: record_expiration(snapshot=%s, bars=<name>)", in.Snapshot, in.Snapshot))
		return &mcp.CallToolResult{Content: text(&b)}, Result{}, nil
	}
	p := store.BarsPath(in.Bars)
	raw, err := fs.ReadFile(src.FS, p)
	if err != nil {
		return nil, Result{}, fmt.Errorf("%s: %w (ingest the bars with role bars, name %s)", p, err, in.Bars)
	}
	series, err := snapshot.DecodeBars(p, raw)
	if err != nil {
		return nil, Result{}, err
	}
	recs, err := report.NewExpirations(&b, todo, series, p)
	if err != nil {
		return nil, Result{}, err
	}
	if err := store.RecordExpirations(ctx, s.d.DB, recs, store.SHA256(raw), s.d.Now()); err != nil {
		return nil, Result{}, err
	}
	s.invalidate()
	fmt.Fprintf(&b, "Recorded %d outcome(s) in %s.\n", len(recs), snapshot.ExpirationsFile)
	return &mcp.CallToolResult{Content: text(&b)}, Result{}, nil
}

type SealIn struct {
	Book     string `json:"book,omitempty" jsonschema:"the book whose accounts the snapshot must complete; default ira-wheel"`
	Snapshot string `json:"snapshot" jsonschema:"snapshot id to check and seal"`
}

func (s *tools) sealSnapshot(ctx context.Context, _ *mcp.CallToolRequest, in SealIn) (*mcp.CallToolResult, Result, error) {
	bc, err := s.bk(in.Book)
	if err != nil {
		return nil, Result{}, err
	}
	s.invalidate()
	src, err := s.source(ctx)
	if err != nil {
		return nil, Result{}, err
	}
	if !slices.Contains(src.Snapshots, in.Snapshot) {
		return nil, Result{}, fmt.Errorf("no snapshot %q; open_snapshot it first", in.Snapshot)
	}
	label := layout.Snapshot(in.Snapshot)
	var b bytes.Buffer
	if ok, err := store.IsSealed(ctx, s.d.DB, in.Snapshot); err != nil {
		return nil, Result{}, err
	} else if ok {
		fmt.Fprintf(&b, "Snapshot %s is complete and matches its manifest.\n", label)
		return &mcp.CallToolResult{Content: text(&b)}, Result{}, nil
	}
	ps, _, err := store.Seal(ctx, s.d.DB, src, in.Snapshot, report.AccountRefs(bc.Cfg), bc.Cfg.Source(), s.d.Now())
	if err != nil {
		return nil, Result{}, err
	}
	if len(ps) > 0 {
		report.SnapshotProblems(&b, label, ps)
		return &mcp.CallToolResult{Content: text(&b)}, Result{ExitCode: 1}, nil
	}
	s.invalidate()
	sealed, err := s.source(ctx)
	if err != nil {
		return nil, Result{}, fmt.Errorf("sealed, but the store no longer opens: %w", err)
	}
	m, err := snapshot.ReadManifestAt(sealed.At(in.Snapshot))
	if err != nil {
		return nil, Result{}, err
	}
	fmt.Fprintf(&b, "Snapshot %s is complete. Wrote manifest.json fingerprinting %d files; the snapshot is now read-only.\n", label, len(m.Files))
	return &mcp.CallToolResult{Content: text(&b)}, Result{}, nil
}
