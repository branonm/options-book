// Package obmcp is the ob-mcp server's tool set (the storage design phase
// 3): the read tools, over the store. Every figure-producing tool calls the
// same internal/report function the `wheel` CLI calls and returns its text
// unchanged, so a tool's output is the CLI's byte for byte; the exit code and
// the run id travel in structured content only.
//
// The server holds no broker credentials and has no order path. Its only
// write is the create-only run document each figure-producing call leaves.
package obmcp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/branonm/options-book/internal/layout"
	"io"
	"io/fs"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/branonm/options-book/internal/broker"
	"github.com/branonm/options-book/internal/config"
	"github.com/branonm/options-book/internal/cred"
	"github.com/branonm/options-book/internal/notes"
	"github.com/branonm/options-book/internal/report"
	"github.com/branonm/options-book/internal/rules"
	"github.com/branonm/options-book/internal/snapshot"
	"github.com/branonm/options-book/internal/store"
)

// Deps is everything the tools need.
// BookConfig is one book's configuration in force on the server, with the
// sha256 of each file it came from (recorded on every run).
type BookConfig struct {
	Name     string
	Cfg      *config.Book
	Rules    *rules.Rules
	BookSHA  string // sha256 of the book.json in force
	RulesSHA string // sha256 of the rules.json in force
}

type Deps struct {
	// Books are the compiled-in books by name; a tool's optional book parameter
	// picks one, DefaultBook when it is empty.
	Books       map[string]*BookConfig
	DefaultBook string
	// Accounts is every account (config/accounts.json), the names a write tool
	// may be given whichever book covers them; AccountsSHA is that file's sha256.
	Accounts    *config.Accounts
	AccountsSHA string
	DB          store.DB
	Spec        string        // how `wheel` names this store, e.g. cloudsql=INSTANCE/DATABASE
	Commit      string        // the server's git commit, recorded on every run
	CacheTTL    time.Duration // how long an opened store is reused; 0 reopens every call
	Now         func() time.Time

	// Phase 5. With Vault nil, fetch_snapshot and cred_status aren't offered.
	Vault         cred.Vault
	Robinhood     cred.Provider // its Authorize pins Robinhood's MCP URL
	AlpacaPaper   cred.Provider
	RobinhoodMCP  string
	RobinhoodHTTP *http.Client
	AlpacaHTTP    *http.Client // nil: a default client

	// Notes is the owner's working notes. With it nil, the notes tools aren't offered.
	Notes notes.Store
}

// bk resolves a tool's book parameter: the named book, or the default when empty.
func (s *tools) bk(name string) (*BookConfig, error) {
	if name == "" {
		name = s.d.DefaultBook
	}
	b, ok := s.d.Books[name]
	if !ok {
		var have []string
		for n := range s.d.Books {
			have = append(have, n)
		}
		sort.Strings(have)
		return nil, fmt.Errorf("no book %q (this server has: %s)", name, strings.Join(have, ", "))
	}
	return b, nil
}

// Instructions are sent to Claude when it connects.
const Instructions = `options-book: the owner's options wheel book, computed by code from stored Robinhood responses. ` +
	`Every figure comes from a tool; quote tool output, never estimate. checkin, book, ledger, ticket, diff and reconcile ` +
	`return exactly what the ob CLI prints for the same snapshot. This server places no orders and holds no broker credentials. ` +
	`Snapshots are named by id, e.g. 2026-11-16 or 2026-11-13T1151; list_snapshots lists them. ` +
	`A check-in through the server: history_plan per account; open_snapshot; ingest every Robinhood response unedited ` +
	`(say verbatim=false with a note if you retyped one); history_add per account; seal_snapshot; then checkin, diff, book. ` +
	`Everything stored is permanent: nothing can be changed or removed, and a sealed snapshot takes no new inputs. ` +
	`The owner's working notes (PLAN.md, BUGS.md, design docs) are read with notes_read and changed with notes_write against the generation just read; earlier generations are kept and nothing is deleted.`

// Register adds every read tool to server.
func Register(server *mcp.Server, d *Deps) {
	if d.Now == nil {
		d.Now = time.Now
	}
	s := &tools{d: d}
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: boolp(false)}
	// Figure-producing tools leave a run document, so they aren't read-only
	// in MCP's sense, but they change nothing and repeat safely.
	audited := &mcp.ToolAnnotations{DestructiveHint: boolp(false), IdempotentHint: true, OpenWorldHint: boolp(false)}

	mcp.AddTool(server, &mcp.Tool{Name: "checkin", Annotations: audited,
		Description: "Check-in for a snapshot: defects (an order is wrong; fix it in Robinhood) and alerts (judgment), the Rule 10 closers table, premium, concentration shares and the ladder. exit_code 1 means defects. Same output as `ob checkin`."},
		expose(s.checkin))
	mcp.AddTool(server, &mcp.Tool{Name: "book", Annotations: audited,
		Description: "The generated wheel-book tables for a snapshot, as Markdown. Same output as `ob book`."},
		expose(s.book))
	mcp.AddTool(server, &mcp.Tool{Name: "ledger", Annotations: audited,
		Description: "Ledger for one account or all: realized premium, open-leg credit, blocks with premium and Rule 11 breakevens, open legs, losing legs. Same output as `wheel ledger`."},
		expose(s.ledger))
	mcp.AddTool(server, &mcp.Tool{Name: "ticket", Annotations: audited,
		Description: "Build a ticket for a proposed trade (covered_call, acquisition_put or income_put) against a contract whose quote and instrument are stored in the snapshot, or a rejection with reasons. exit_code 1 means rejected. Places no order. Same output as `ob ticket`."},
		expose(s.ticket))
	mcp.AddTool(server, &mcp.Tool{Name: "diff", Annotations: audited,
		Description: "What changed between two snapshots: fills, new and cancelled orders, legs opened and closed, realized premium, breakevens, findings resolved and new. Same output as `ob diff`."},
		expose(s.diff))
	mcp.AddTool(server, &mcp.Tool{Name: "reconcile", Annotations: audited,
		Description: "Ledger realized premium vs Robinhood's own realized option P&L, from the get_realized_pnl responses stored with the snapshot. exit_code 1 means a mismatch over $1. Same output as `ob reconcile`."},
		expose(s.reconcile))
	mcp.AddTool(server, &mcp.Tool{Name: "list_snapshots", Annotations: ro,
		Description: "Every stored snapshot, when it was sealed, and how many files it holds. Same output as `ob db list`."},
		expose(s.listSnapshots))
	mcp.AddTool(server, &mcp.Tool{Name: "get_snapshot", Annotations: ro,
		Description: "One snapshot's files with sizes and sha256 and its seal state; name files in raw (e.g. roth/portfolio.json) to get their stored bytes verbatim. Same output as `ob db show`."},
		expose(s.getSnapshot))
	mcp.AddTool(server, &mcp.Tool{Name: "verify", Annotations: ro,
		Description: "Re-check the whole store: every body's sha256, both order-log hash chains, every position and seal, and that export → import → export is byte-identical. Same output as `ob db verify`."},
		expose(s.verify))
	registerWrites(server, s)
	if d.Vault != nil {
		registerFetch(server, s)
	}
	if d.Notes != nil {
		registerNotes(server, s)
	}
}

func boolp(b bool) *bool { return &b }

type tools struct {
	d      *Deps
	mu     sync.Mutex
	src    *store.Source
	opened time.Time
}

// source opens and verifies the store, reusing it for CacheTTL. Every open
// re-checks every hash, chain and seal.
func (s *tools) source(ctx context.Context) (*store.Source, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.src != nil && s.d.Now().Sub(s.opened) < s.d.CacheTTL {
		return s.src, nil
	}
	src, err := store.Open(ctx, s.d.DB)
	if err != nil {
		return nil, err
	}
	s.src, s.opened = src, s.d.Now()
	return src, nil
}

func (s *tools) at(src *store.Source, id string) (snapshot.At, error) {
	if !slices.Contains(src.Snapshots, id) {
		return snapshot.At{}, fmt.Errorf("no snapshot %q; list_snapshots lists them", id)
	}
	return src.At(id), nil
}

// Result is every tool's structured output. It carries the whole report:
// the Claude connector shows a tool's structuredContent in place of its text
// content (B7), so Output is the text content verbatim, and a refusal's
// reason is in Error (exit_code 2, as the CLI uses for errors).
type Result struct {
	ExitCode int    `json:"exit_code"`
	RunID    string `json:"run_id,omitempty"`
	SHA256   string `json:"sha256,omitempty"` // a stored Robinhood response (rh_read)
	Output   string `json:"output"`
	Error    string `json:"error,omitempty"`
	// Generation is a note's generation number (notes_read, notes_write), as a string.
	Generation string `json:"generation,omitempty"`
}

// expose makes a handler's report and its refusals visible to any client,
// whichever part of the result it reads (B7): the text goes in both content
// and Result.Output, and an error becomes an IsError result whose reason is in
// both content and Result.Error.
func expose[In any](h func(context.Context, *mcp.CallToolRequest, In) (*mcp.CallToolResult, Result, error)) mcp.ToolHandlerFor[In, Result] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Result, error) {
		res, out, err := h(ctx, req, in)
		if err != nil {
			msg := err.Error()
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}, Result{ExitCode: 2, Error: msg}, nil
		}
		if res != nil {
			for _, c := range res.Content {
				if tc, ok := c.(*mcp.TextContent); ok {
					out.Output += tc.Text
				}
			}
		}
		return res, out, nil
	}
}

func text(b *bytes.Buffer) []mcp.Content { return []mcp.Content{&mcp.TextContent{Text: b.String()}} }

// figures runs one figure-producing report and records the run.
func (s *tools) figures(ctx context.Context, tool string, args any, bk *BookConfig, snapID string, f func(w io.Writer, src *store.Source) (int, error)) (*mcp.CallToolResult, Result, error) {
	src, err := s.source(ctx)
	if err != nil {
		return nil, Result{}, err
	}
	var b bytes.Buffer
	code, err := f(&b, src)
	if err != nil {
		return nil, Result{}, err
	}
	id, err := store.RecordRun(ctx, s.d.DB, store.Run{
		Tool: tool, Args: args, SnapshotID: snapID, Book: bk.Name, RulesSHA: bk.RulesSHA, BookSHA: bk.BookSHA, AccountsSHA: s.d.AccountsSHA,
		Commit: s.d.Commit, Output: b.Bytes(), ExitCode: code, RanAt: s.d.Now(),
	})
	if err != nil {
		// A figure without its audit record is not returned.
		return nil, Result{}, err
	}
	return &mcp.CallToolResult{Content: text(&b)}, Result{ExitCode: code, RunID: id}, nil
}

type SnapshotIn struct {
	Book     string `json:"book,omitempty" jsonschema:"the book to run, e.g. ira-wheel; default ira-wheel"`
	Snapshot string `json:"snapshot" jsonschema:"snapshot id, e.g. 2026-11-16 or 2026-11-13T1151"`
	AsOf     string `json:"asof,omitempty" jsonschema:"as-of date YYYY-MM-DD; default the snapshot id's session date"`
	JSON     bool   `json:"json,omitempty" jsonschema:"machine-readable JSON instead of text, as the CLI's --json"`
}

func (s *tools) checkin(ctx context.Context, _ *mcp.CallToolRequest, in SnapshotIn) (*mcp.CallToolResult, Result, error) {
	bk, err := s.bk(in.Book)
	if err != nil {
		return nil, Result{}, err
	}
	return s.figures(ctx, "checkin", in, bk, in.Snapshot, func(w io.Writer, src *store.Source) (int, error) {
		at, asOf, err := s.dated(src, in.Snapshot, in.AsOf)
		if err != nil {
			return 0, err
		}
		return report.Checkin(w, bk.Cfg, bk.Rules, at, asOf, in.JSON)
	})
}

type BookIn struct {
	Book     string `json:"book,omitempty" jsonschema:"the book to run, e.g. ira-wheel; default ira-wheel"`
	Snapshot string `json:"snapshot" jsonschema:"snapshot id, e.g. 2026-11-16"`
	AsOf     string `json:"asof,omitempty" jsonschema:"as-of date YYYY-MM-DD; default the snapshot id's session date"`
}

func (s *tools) book(ctx context.Context, _ *mcp.CallToolRequest, in BookIn) (*mcp.CallToolResult, Result, error) {
	bk, err := s.bk(in.Book)
	if err != nil {
		return nil, Result{}, err
	}
	return s.figures(ctx, "book", in, bk, in.Snapshot, func(w io.Writer, src *store.Source) (int, error) {
		at, asOf, err := s.dated(src, in.Snapshot, in.AsOf)
		if err != nil {
			return 0, err
		}
		return 0, report.Book(w, bk.Cfg, bk.Rules, at, asOf, at.Dir)
	})
}

type LedgerIn struct {
	Book     string `json:"book,omitempty" jsonschema:"the book to run, e.g. ira-wheel; default ira-wheel"`
	Snapshot string `json:"snapshot" jsonschema:"snapshot id, e.g. 2026-11-16"`
	Account  string `json:"account,omitempty" jsonschema:"one account (e.g. roth); default all"`
	AsOf     string `json:"asof,omitempty" jsonschema:"as-of date YYYY-MM-DD; default the snapshot id's session date"`
	JSON     bool   `json:"json,omitempty" jsonschema:"machine-readable JSON instead of text"`
}

func (s *tools) ledger(ctx context.Context, _ *mcp.CallToolRequest, in LedgerIn) (*mcp.CallToolResult, Result, error) {
	bk, err := s.bk(in.Book)
	if err != nil {
		return nil, Result{}, err
	}
	return s.figures(ctx, "ledger", in, bk, in.Snapshot, func(w io.Writer, src *store.Source) (int, error) {
		at, asOf, err := s.dated(src, in.Snapshot, in.AsOf)
		if err != nil {
			return 0, err
		}
		return 0, report.Ledger(w, bk.Cfg, at, asOf, in.Account, in.JSON)
	})
}

type TicketIn struct {
	Book     string `json:"book,omitempty" jsonschema:"the book to run, e.g. ira-wheel; default ira-wheel"`
	Snapshot string `json:"snapshot,omitempty" jsonschema:"snapshot id for positions and blocks; with option, it also holds the contract's stored quote and instrument. With contract, default the latest sealed snapshot"`
	Account  string `json:"account" jsonschema:"account name, e.g. roth"`
	Kind     string `json:"kind" jsonschema:"covered_call, acquisition_put or income_put"`
	Contract string `json:"contract,omitempty" jsonschema:"OCC symbol, e.g. AAA   270115C00076000: the server fetches the contract and its quote from Robinhood itself"`
	Option   string `json:"option,omitempty" jsonschema:"instead of contract: an option instrument id stored under the snapshot's options/"`
	Limit    string `json:"limit" jsonschema:"limit price per share, decimal string, e.g. 0.80"`
	Qty      int64  `json:"qty,omitempty" jsonschema:"contracts; default every free lot for a call, 1 for a put"`
	AsOf     string `json:"asof,omitempty" jsonschema:"as-of date YYYY-MM-DD; default the snapshot id's session date"`
	JSON     bool   `json:"json,omitempty" jsonschema:"machine-readable JSON instead of text"`
}

func (s *tools) ticket(ctx context.Context, _ *mcp.CallToolRequest, in TicketIn) (*mcp.CallToolResult, Result, error) {
	if in.Contract != "" {
		return s.ticketContract(ctx, in)
	}
	if in.Snapshot == "" || in.Option == "" {
		return nil, Result{}, fmt.Errorf("give contract (an OCC symbol), or snapshot and option")
	}
	bk, err := s.bk(in.Book)
	if err != nil {
		return nil, Result{}, err
	}
	return s.figures(ctx, "ticket", in, bk, in.Snapshot, func(w io.Writer, src *store.Source) (int, error) {
		at, asOf, err := s.dated(src, in.Snapshot, in.AsOf)
		if err != nil {
			return 0, err
		}
		req := report.TicketRequest{Account: in.Account, Kind: in.Kind, Option: in.Option, Limit: in.Limit, Qty: in.Qty}
		return report.Ticket(w, bk.Cfg, bk.Rules, at, asOf, req, in.JSON)
	})
}

type DiffIn struct {
	Book string `json:"book,omitempty" jsonschema:"the book to run, e.g. ira-wheel; default ira-wheel"`
	From string `json:"from" jsonschema:"earlier snapshot id"`
	To   string `json:"to" jsonschema:"later snapshot id"`
}

func (s *tools) diff(ctx context.Context, _ *mcp.CallToolRequest, in DiffIn) (*mcp.CallToolResult, Result, error) {
	bk, err := s.bk(in.Book)
	if err != nil {
		return nil, Result{}, err
	}
	return s.figures(ctx, "diff", in, bk, in.To, func(w io.Writer, src *store.Source) (int, error) {
		if err := report.CheckOrder(in.From, in.To); err != nil {
			return 0, err
		}
		a, err := s.at(src, in.From)
		if err != nil {
			return 0, err
		}
		b, err := s.at(src, in.To)
		if err != nil {
			return 0, err
		}
		return 0, report.Diff(w, bk.Cfg, bk.Rules, a, b)
	})
}

type ReconcileIn struct {
	Book     string   `json:"book,omitempty" jsonschema:"the book to run, e.g. ira-wheel; default ira-wheel"`
	Snapshot string   `json:"snapshot" jsonschema:"snapshot id whose stored get_realized_pnl responses to reconcile against"`
	Accounts []string `json:"accounts,omitempty" jsonschema:"accounts to reconcile, in order; default every configured account with a stored response"`
	AsOf     string   `json:"asof,omitempty" jsonschema:"as-of date YYYY-MM-DD; default the snapshot id's session date"`
	JSON     bool     `json:"json,omitempty" jsonschema:"machine-readable JSON instead of text"`
}

// PnLPath is where a snapshot's realized-P&L response sits in the repo layout.
func PnLPath(snap, acct string) string {
	return layout.PnL(snap, acct)
}

// readPnL reads and decodes one stored get_realized_pnl response.
func readPnL(fsys fs.FS, name string) (broker.RealizedPnL, error) {
	b, err := fs.ReadFile(fsys, name)
	if err != nil {
		return broker.RealizedPnL{}, err
	}
	return snapshot.DecodeRealizedPnL(name, b)
}

func (s *tools) reconcile(ctx context.Context, _ *mcp.CallToolRequest, in ReconcileIn) (*mcp.CallToolResult, Result, error) {
	bk, err := s.bk(in.Book)
	if err != nil {
		return nil, Result{}, err
	}
	return s.figures(ctx, "reconcile", in, bk, in.Snapshot, func(w io.Writer, src *store.Source) (int, error) {
		at, asOf, err := s.dated(src, in.Snapshot, in.AsOf)
		if err != nil {
			return 0, err
		}
		accts := in.Accounts
		explicit := len(accts) > 0
		if !explicit {
			for _, a := range bk.Cfg.Accounts {
				accts = append(accts, a.Name)
			}
		}
		var pnls []report.PnL
		for _, a := range accts {
			resp, err := readPnL(src.FS, PnLPath(in.Snapshot, a))
			switch {
			case err == nil:
				pnls = append(pnls, report.PnL{Account: a, Resp: resp})
			case explicit || !errors.Is(err, fs.ErrNotExist):
				return 0, err
			}
		}
		if len(pnls) == 0 {
			return 0, fmt.Errorf("no get_realized_pnl response is stored for snapshot %s", in.Snapshot)
		}
		return report.Reconcile(w, bk.Cfg, at, asOf, pnls, in.JSON)
	})
}

func (s *tools) dated(src *store.Source, id, asof string) (snapshot.At, time.Time, error) {
	at, err := s.at(src, id)
	if err != nil {
		return snapshot.At{}, time.Time{}, err
	}
	asOf, err := report.ResolveAsOf(id, asof)
	return at, asOf, err
}

type NoIn struct{}

func (s *tools) listSnapshots(ctx context.Context, _ *mcp.CallToolRequest, _ NoIn) (*mcp.CallToolResult, Result, error) {
	src, err := s.source(ctx)
	if err != nil {
		return nil, Result{}, err
	}
	var b bytes.Buffer
	if err := report.ListSnapshots(&b, src.FS, src.Snapshots); err != nil {
		return nil, Result{}, err
	}
	return &mcp.CallToolResult{Content: text(&b)}, Result{}, nil
}

type GetSnapshotIn struct {
	Snapshot string   `json:"snapshot" jsonschema:"snapshot id"`
	Raw      []string `json:"raw,omitempty" jsonschema:"files to return verbatim, relative to the snapshot, e.g. roth/portfolio.json"`
}

func (s *tools) getSnapshot(ctx context.Context, _ *mcp.CallToolRequest, in GetSnapshotIn) (*mcp.CallToolResult, Result, error) {
	src, err := s.source(ctx)
	if err != nil {
		return nil, Result{}, err
	}
	at, err := s.at(src, in.Snapshot)
	if err != nil {
		return nil, Result{}, err
	}
	var b bytes.Buffer
	if err := report.GetSnapshot(&b, at, in.Raw); err != nil {
		return nil, Result{}, err
	}
	return &mcp.CallToolResult{Content: text(&b)}, Result{}, nil
}

func (s *tools) verify(ctx context.Context, _ *mcp.CallToolRequest, _ NoIn) (*mcp.CallToolResult, Result, error) {
	rep, err := store.Verify(ctx, s.d.DB)
	if err != nil {
		return nil, Result{}, err
	}
	b := bytes.NewBufferString(rep.Summary(s.d.Spec))
	return &mcp.CallToolResult{Content: text(b)}, Result{}, nil
}
