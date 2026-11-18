package obmcp

// fetch_snapshot for an Alpaca book (PLAN.md item 6, step 4, the Fetcher): the
// same open → pull → history_add → seal flow as the Robinhood pull, read from
// Alpaca's paper API and market-data API through internal/alpacapull's GET-only
// allowlist, every response stored verbatim. The key is read from the
// alpaca-paper secret and sent only where cred.AlpacaPaper.Authorize allows.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/branonm/options-book/internal/alpacapull"
	"github.com/branonm/options-book/internal/broker"
	"github.com/branonm/options-book/internal/cred"
	"github.com/branonm/options-book/internal/history"
	"github.com/branonm/options-book/internal/layout"
	"github.com/branonm/options-book/internal/report"
	"github.com/branonm/options-book/internal/store"
)

// AlpacaSource is the raw_response source of a server-side Alpaca pull.
const AlpacaSource = "alpaca/paper-rest"

const alpacaBroker = "alpaca"

func (s *tools) alpacaClient(ctx context.Context) (*alpacapull.Client, string, error) {
	if s.d.Vault == nil || s.d.AlpacaPaper == nil {
		return nil, "", errors.New("this server has no Alpaca paper credential configured")
	}
	secret, err := s.d.Vault.Latest(ctx, "alpaca-paper")
	if err != nil {
		return nil, "", fmt.Errorf("reading the alpaca-paper credential (ob cred enroll alpaca-paper): %w", err)
	}
	st, err := cred.Describe(ctx, s.d.Vault, s.d.AlpacaPaper, s.d.Now())
	if err != nil {
		return nil, "", err
	}
	return &alpacapull.Client{
		HC: s.d.AlpacaHTTP, Now: s.d.Now,
		Authorize: func(ctx context.Context, req *http.Request) error {
			return s.d.AlpacaPaper.Authorize(ctx, secret.Data, req)
		},
	}, st.String(), nil
}

type alpacaFetcher struct {
	t     *tools
	c     *alpacapull.Client
	id    string
	acct  string
	log   *bytes.Buffer
	pulls [][]string
	prov  []string
}

func (f *alpacaFetcher) put(ctx context.Context, r *alpacapull.Response, role, key string) error {
	args, err := json.Marshal(r.Args())
	if err != nil {
		return err
	}
	acct := f.acct
	if role == store.RoleEquityQuotes || role == store.RoleOptionQuotes {
		acct = ""
	}
	res, err := store.IngestResponse(ctx, f.t.d.DB, store.Ingest{
		Snapshot: f.id, Role: role, Account: acct, Instrument: key, Call: r.Call(), Args: string(args),
		Body: r.Body, Verbatim: true, FetchedAt: r.FetchedAt.Format(time.RFC3339Nano), Source: AlpacaSource, Broker: alpacaBroker,
	}, f.t.d.Now())
	if err != nil {
		return fmt.Errorf("storing %s: %w", r.Call(), err)
	}
	rel := strings.TrimPrefix(res.Path, layout.Snapshot(f.id)+"/")
	f.prov = append(f.prov, fmt.Sprintf("| `%s` | `%s %s` | %s | `%s…` |", rel, r.Call(), r.Query.Encode(), r.FetchedAt.Format(time.RFC3339), res.SHA256[:12]))
	fmt.Fprintf(f.log, "  %-38s %s %s, %d bytes\n", rel, r.Call(), r.Query.Encode(), res.Bytes)
	return nil
}

// ordersFrom pulls status=all pages from after (RFC3339 or a date, "" for the
// whole history) until a page is short, storing each as an orders_pull page.
// It returns the page names and the order ids seen.
func (f *alpacaFetcher) ordersFrom(ctx context.Context, prefix, after string, seen map[string]bool, first *time.Time) ([]string, error) {
	var names []string
	for i := 1; ; i++ {
		r, err := f.c.Orders(ctx, "all", after)
		if err != nil {
			return nil, err
		}
		name := fmt.Sprintf("%s_p%d", prefix, i)
		if err := f.put(ctx, r, store.RoleOrdersPull, name); err != nil {
			return nil, err
		}
		names = append(names, name)
		if i == 1 && first != nil {
			*first = r.FetchedAt.Truncate(time.Second)
		}
		records, next, err := history.PageRecordsFor(name, r.Body, alpacaBroker)
		if err != nil {
			return nil, err
		}
		for _, raw := range records {
			h, err := history.OrderHeadFor(raw, alpacaBroker)
			if err != nil {
				return nil, err
			}
			seen[h.ID] = true
		}
		if next == "" {
			return names, nil
		}
		if next == after {
			return nil, fmt.Errorf("%s: %d orders share the created_at %s, so the pull cannot page past them", name, alpacapull.OrdersPageLimit, next)
		}
		after = next
	}
}

func (s *tools) fetchSnapshotAlpaca(ctx context.Context, in FetchIn, bc *BookConfig, log *bytes.Buffer) (*mcp.CallToolResult, Result, error) {
	if len(bc.Cfg.Accounts) != 1 {
		return nil, Result{}, fmt.Errorf("book %s has %d accounts; an Alpaca key is one account, so a pull needs exactly one", bc.Name, len(bc.Cfg.Accounts))
	}
	c, st, err := s.alpacaClient(ctx)
	if err != nil {
		return nil, Result{}, err
	}
	acct := bc.Cfg.Accounts[0].Name
	fmt.Fprintf(log, "Alpaca paper credential: %s\n", st)
	fmt.Fprintf(log, "Book %s: pulling account %s from Alpaca's paper API.\n", bc.Name, acct)
	f := &alpacaFetcher{t: s, c: c, id: in.Snapshot, acct: acct, log: log}

	// Before anything is stored: the key works.
	account, err := c.Account(ctx)
	if err != nil {
		return nil, Result{}, fetchErr(log, in.Snapshot, false, err)
	}
	if err := checkAlpacaAccount(account.Body, acct, bc.Cfg.Accounts[0].Last4); err != nil {
		return nil, Result{}, fetchErr(log, in.Snapshot, false, err)
	}
	note := in.Note
	if note == "" {
		note = "pulled by fetch_snapshot"
	}
	if err := store.OpenSnapshot(ctx, s.d.DB, in.Snapshot, note, s.d.Now()); err != nil {
		return nil, Result{}, fetchErr(log, in.Snapshot, false, err)
	}
	s.invalidate()
	fmt.Fprintf(log, "Opened snapshot %s.\n", in.Snapshot)
	if err := f.fill(ctx, account); err != nil {
		s.invalidate()
		return nil, Result{}, fetchErr(log, in.Snapshot, true, err)
	}
	s.invalidate()
	src, err := s.source(ctx)
	if err != nil {
		return nil, Result{}, fetchErr(log, in.Snapshot, true, err)
	}
	ps, sealRes, err := store.Seal(ctx, s.d.DB, src, in.Snapshot, report.AccountRefs(bc.Cfg), alpacaBroker, s.d.Now())
	s.invalidate()
	if err != nil {
		return nil, Result{}, fetchErr(log, in.Snapshot, true, err)
	}
	if len(ps) > 0 {
		var pb bytes.Buffer
		report.SnapshotProblems(&pb, layout.Snapshot(in.Snapshot), ps)
		return nil, Result{}, fetchErr(log, in.Snapshot, true, fmt.Errorf("the snapshot isn't complete, so it wasn't sealed:\n%s", pb.String()))
	}
	fmt.Fprintf(log, "Sealed snapshot %s (%d members and positions). Every response above is stored verbatim from Alpaca.\n", in.Snapshot, sealRes.Members)
	return &mcp.CallToolResult{Content: text(log)}, Result{}, nil
}

func (f *alpacaFetcher) fill(ctx context.Context, account *alpacapull.Response) error {
	src, err := f.t.source(ctx)
	if err != nil {
		return err
	}
	l, err := history.LoadFSFor(src.FS, store.LogPath(f.acct), alpacaBroker)
	if err != nil {
		return err
	}
	full := len(l.Entries) == 0
	plan := l.NextPlan()
	var observed time.Time
	seen := map[string]bool{}
	var pulls [][]string
	if full {
		fmt.Fprintf(f.log, "%s: the order log is empty, so this is a full history pull.\n", f.acct)
		names, err := f.ordersFrom(ctx, "full", "", seen, &observed)
		if err != nil {
			return err
		}
		pulls = append(pulls, names)
	} else {
		fmt.Fprintf(f.log, "%s (log %d lines; history_plan: created since %s, %d open in the log):\n", f.acct, len(l.Entries), plan.CreatedSince, len(plan.RefreshIDs))
		names, err := f.ordersFrom(ctx, "window", plan.CreatedSince+"T00:00:00Z", seen, &observed)
		if err != nil {
			return err
		}
		pulls = append(pulls, names)
		r, err := f.c.Orders(ctx, "open", "")
		if err != nil {
			return err
		}
		if err := f.put(ctx, r, store.RoleOrdersPull, "open_p1"); err != nil {
			return err
		}
		records, next, err := history.PageRecordsFor("open_p1", r.Body, alpacaBroker)
		if err != nil {
			return err
		}
		if next != "" {
			return fmt.Errorf("open_p1 is a full page of %d open orders; the pull cannot confirm it holds them all", alpacapull.OrdersPageLimit)
		}
		for _, raw := range records {
			h, err := history.OrderHeadFor(raw, alpacaBroker)
			if err != nil {
				return err
			}
			seen[h.ID] = true
		}
		pulls = append(pulls, []string{"open_p1"})
		for _, id := range plan.RefreshIDs {
			if seen[id] {
				continue
			}
			r, err := f.c.Order(ctx, id)
			if err != nil {
				return err
			}
			name := "lookup_" + id[:min(8, len(id))]
			if err := f.put(ctx, r, store.RoleOrdersPull, name); err != nil {
				return err
			}
			pulls = append(pulls, []string{name})
		}
	}

	dec, err := broker.Lookup(alpacaBroker)
	if err != nil {
		return err
	}
	pos, err := f.c.Positions(ctx)
	if err != nil {
		return err
	}
	// One response, two roles: Alpaca's positions list holds shares and options.
	if err := f.put(ctx, pos, store.RoleEquityPositions, ""); err != nil {
		return err
	}
	if err := f.put(ctx, pos, store.RoleOptionPositions, ""); err != nil {
		return err
	}
	if err := f.put(ctx, account, store.RolePortfolio, ""); err != nil {
		return err
	}
	eqs, err := dec.EquityPositions("positions", pos.Body)
	if err != nil {
		return err
	}
	ops, err := dec.OptionPositions("positions", pos.Body)
	if err != nil {
		return err
	}
	symbols, options := map[string]bool{}, map[string]bool{}
	for _, p := range eqs {
		symbols[p.Symbol] = true
	}
	for _, p := range ops {
		options[p.InstrumentID] = true
	}

	fmt.Fprintln(f.log, "Quotes:")
	if len(symbols) > 0 {
		r, err := f.c.LatestTrades(ctx, sorted(symbols))
		if err != nil {
			return err
		}
		if err := f.put(ctx, r, store.RoleEquityQuotes, ""); err != nil {
			return err
		}
	}
	if len(options) > 0 {
		r, err := f.c.OptionSnapshots(ctx, sorted(options))
		if err != nil {
			return err
		}
		if err := f.put(ctx, r, store.RoleOptionQuotes, ""); err != nil {
			return err
		}
	}

	f.t.invalidate()
	src, err = f.t.source(ctx)
	if err != nil {
		return err
	}
	add := store.HistoryAdd{Snapshot: f.id, Account: f.acct, ObservedAt: observed, Pulls: pulls, Broker: alpacaBroker, Full: full}
	if !full {
		add.CreatedSince, _ = time.Parse("2006-01-02", plan.CreatedSince)
	}
	res, err := store.AddHistory(ctx, f.t.d.DB, src, add)
	if err != nil {
		return fmt.Errorf("history_add %s: %w", f.acct, err)
	}
	f.t.invalidate()
	fmt.Fprintf(f.log, "%s: %d new orders, %d changed, %d unchanged; appended %d lines (log now %d).\n",
		f.acct, res.NewOrders, res.Changed, res.Unchanged, res.Appended, res.Lines)

	sort.Strings(f.prov)
	prov := fmt.Sprintf("# Snapshot %s — provenance\n\nPulled by `fetch_snapshot` (ob-mcp %s) from Alpaca's paper-trading API and market-data API through the GET-only allowlist. "+
		"Every response is stored **verbatim**: the HTTP body, byte for byte.\n\n"+
		"| File | Call | Fetched (UTC) | sha256 |\n|---|---|---|---|\n%s\n", f.id, f.t.d.Commit, strings.Join(f.prov, "\n"))
	if _, err := store.IngestResponse(ctx, f.t.d.DB, store.Ingest{
		Snapshot: f.id, Role: store.RoleProvenance, Body: []byte(prov), Verbatim: true,
	}, f.t.d.Now()); err != nil {
		return fmt.Errorf("storing PROVENANCE.md: %w", err)
	}
	return nil
}

// checkAlpacaAccount confirms the key reaches the account config/accounts.json
// names: the last four characters of the account's account_number must equal
// the configured last4. A key for a different account, or a response with no
// account_number, is refused before anything is stored. The error shows only
// the last four, never the number.
func checkAlpacaAccount(body []byte, name, last4 string) error {
	var a struct {
		AccountNumber string `json:"account_number"`
	}
	if err := json.Unmarshal(body, &a); err != nil {
		return fmt.Errorf("account %s: reading GET /v2/account: %w", name, err)
	}
	n := a.AccountNumber
	if n == "" {
		return fmt.Errorf("account %s: GET /v2/account returned no account_number, so last4 %s can't be confirmed", name, last4)
	}
	if !strings.HasSuffix(n, last4) {
		got := n
		if len(got) > 4 {
			got = got[len(got)-4:]
		}
		return fmt.Errorf("account %s: the Alpaca key's account ends in %s, but config/accounts.json says last4 %s; wrong key or wrong config", name, got, last4)
	}
	return nil
}
