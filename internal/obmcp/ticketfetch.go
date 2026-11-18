package obmcp

// ticket by contract (PLAN.md, addendum A): the server resolves an OCC
// symbol through Robinhood itself — get_option_chains → get_option_instruments
// → get_option_quotes — stores each response verbatim, and computes the
// ticket from the stored bytes, read back. Positions and blocks come from the
// given snapshot or the latest sealed one. The quote window is checked against
// the server's own fetch time.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/shopspring/decimal"

	"github.com/branonm/options-book/internal/alpacapull"
	"github.com/branonm/options-book/internal/broker"
	"github.com/branonm/options-book/internal/broker/robinhood"
	"github.com/branonm/options-book/internal/occ"
	"github.com/branonm/options-book/internal/report"
	"github.com/branonm/options-book/internal/rhmcp"
	"github.com/branonm/options-book/internal/snapshot"
	"github.com/branonm/options-book/internal/store"
)

// arg shapes a value the way a tool's schema asks: a list if it takes an
// array, else the value itself.
func arg(ctx context.Context, rc *rhmcp.Client, tool, name, value string) (any, bool, error) {
	p, ok, err := rc.Property(ctx, tool, name)
	if err != nil || !ok {
		return nil, ok, err
	}
	if p["type"] == "array" {
		return []string{value}, true, nil
	}
	return value, true, nil
}

// fetched is one stored response the ticket read.
type fetched struct {
	tool string
	sha  string
	at   time.Time
}

func (s *tools) storeRaw(ctx context.Context, r *rhmcp.Response) (fetched, []byte, error) {
	args, _ := json.Marshal(r.Args)
	sha, _, err := store.StoreRaw(ctx, s.d.DB, store.Raw{Call: r.Tool, Args: string(args), Body: r.Body, FetchedAt: r.FetchedAt, Source: RobinhoodSource})
	if err != nil {
		return fetched{}, nil, err
	}
	// The ticket is computed from what was stored, not from what was received.
	body, err := store.RawBody(ctx, s.d.DB, sha)
	return fetched{tool: r.Tool, sha: sha, at: r.FetchedAt}, body, err
}

// resolveContract finds the contract's instrument and quote through Robinhood.
func (s *tools) resolveContract(ctx context.Context, rc *rhmcp.Client, c occ.Contract) (*snapshot.Option, []fetched, error) {
	var got []fetched

	// 1. The chain, by whichever symbol input the tool takes.
	var chainArgs map[string]any
	for _, name := range []string{"chain_symbol", "symbol", "symbols", "equity_symbol", "underlying_symbol"} {
		v, ok, err := arg(ctx, rc, "get_option_chains", name, c.Root)
		if err != nil {
			return nil, nil, err
		}
		if ok {
			chainArgs = map[string]any{name: v}
			break
		}
	}
	if chainArgs == nil {
		params, _ := rc.Params(ctx, "get_option_chains")
		return nil, nil, fmt.Errorf("get_option_chains takes none of the symbol inputs this server knows (it takes %v)", params)
	}
	r, err := rc.Call(ctx, "get_option_chains", chainArgs)
	if err != nil {
		return nil, nil, err
	}
	f, _, err := s.storeRaw(ctx, r)
	if err != nil {
		return nil, nil, err
	}
	got = append(got, f)

	// 2. The instrument: chain_symbol, the expiry and the type, as the connector
	// called it (snapshot 2026-11-16's PROVENANCE.md); every page followed.
	instArgs := map[string]any{}
	for name, value := range map[string]string{"chain_symbol": c.Root, "expiration_dates": c.Expiry, "type": c.Type, "strike_price": c.Strike.StringFixed(4)} {
		v, ok, err := arg(ctx, rc, "get_option_instruments", name, value)
		if err != nil {
			return nil, nil, err
		}
		if ok {
			instArgs[name] = v
		}
	}
	if instArgs["chain_symbol"] == nil {
		params, _ := rc.Params(ctx, "get_option_instruments")
		return nil, nil, fmt.Errorf("get_option_instruments doesn't take chain_symbol (it takes %v)", params)
	}
	pages, err := rc.Pages(ctx, "get_option_instruments", instArgs)
	if err != nil {
		return nil, nil, err
	}
	var matches []broker.Instrument
	for _, p := range pages {
		f, body, err := s.storeRaw(ctx, p)
		if err != nil {
			return nil, nil, err
		}
		got = append(got, f)
		instruments, err := robinhood.InstrumentsPage("get_option_instruments", body)
		if err != nil {
			return nil, nil, err
		}
		for _, in := range instruments {
			strike, err := decimal.NewFromString(in.Strike.Text)
			if err == nil && in.Symbol == c.Root && in.Expiry == c.Expiry && in.Right == c.Type && strike.Equal(c.Strike) {
				matches = append(matches, in)
			}
		}
	}
	if len(matches) != 1 {
		return nil, nil, fmt.Errorf("%s: Robinhood returned %d instruments for this contract; want exactly one", c, len(matches))
	}
	inst := matches[0]

	// 3. Its quote.
	r, err = rc.Call(ctx, "get_option_quotes", map[string]any{"instrument_ids": []string{inst.ID}})
	if err != nil {
		return nil, nil, err
	}
	f, body, err := s.storeRaw(ctx, r)
	if err != nil {
		return nil, nil, err
	}
	got = append(got, f)
	dec, err := snapshot.Decoder()
	if err != nil {
		return nil, nil, err
	}
	qs, err := dec.OptionQuotes("get_option_quotes", body)
	if err != nil {
		return nil, nil, err
	}
	if len(qs) != 1 || qs[0].InstrumentID != inst.ID {
		return nil, nil, fmt.Errorf("%s: get_option_quotes didn't return exactly this instrument's quote", c)
	}
	return &snapshot.Option{Quote: qs[0], Instrument: inst}, got, nil
}

// ticketByContract is the ticket tool given an OCC contract.
func (s *tools) ticketByContract(ctx context.Context, in TicketIn) (*bytes.Buffer, int, string, []string, error) {
	bc, err := s.bk(in.Book)
	if err != nil {
		return nil, 0, "", nil, err
	}
	if in.Option != "" || in.AsOf != "" {
		return nil, 0, "", nil, fmt.Errorf("with contract, give neither option nor asof: the server fetches the contract and dates the ticket by when it fetched the quote")
	}
	c, err := occ.Parse(in.Contract)
	if err != nil {
		return nil, 0, "", nil, err
	}
	snapID, sealedAt := in.Snapshot, ""
	if snapID == "" {
		if snapID, sealedAt, err = store.LatestSealed(ctx, s.d.DB); err != nil {
			return nil, 0, "", nil, err
		}
	}
	var (
		opt *snapshot.Option
		got []fetched
	)
	brokerName := "Robinhood"
	if bc.Cfg.Source() == alpacaBroker {
		brokerName = "Alpaca"
		opt, got, err = s.resolveContractAlpaca(ctx, c)
	} else {
		var rc *rhmcp.Client
		if rc, _, err = s.robinhood(ctx); err != nil {
			return nil, 0, "", nil, err
		}
		opt, got, err = s.resolveContract(ctx, rc, c)
	}
	if err != nil {
		return nil, 0, snapID, nil, err
	}
	quoteAt := got[len(got)-1].at
	src, err := s.source(ctx)
	if err != nil {
		return nil, 0, snapID, nil, err
	}
	at, err := s.at(src, snapID)
	if err != nil {
		return nil, 0, snapID, nil, err
	}
	// The as-of is the session the server fetched the quote in.
	ny, _ := time.LoadLocation(bc.Rules.QuoteWindow.Timezone)
	lt := quoteAt.In(ny)
	asOf := time.Date(lt.Year(), lt.Month(), lt.Day(), 0, 0, 0, 0, time.UTC)

	var b bytes.Buffer
	var shas []string
	for _, f := range got {
		shas = append(shas, f.sha)
	}
	if !in.JSON {
		fmt.Fprintf(&b, "Contract %s: instrument %s, quoted by %s at %s (fetched by this server).\n", c, opt.Instrument.ID, brokerName, lt.Format("Mon 2006-01-02 15:04:05 MST"))
		for _, f := range got {
			fmt.Fprintf(&b, "  stored %s verbatim, sha256 %s…\n", f.tool, f.sha[:12])
		}
		id, _ := snapshot.ParseID(snapID)
		if sealedAt != "" {
			fmt.Fprintf(&b, "Positions and blocks: snapshot %s, the latest sealed (%s).\n", snapID, sealedAt)
		} else {
			fmt.Fprintf(&b, "Positions and blocks: snapshot %s.\n", snapID)
		}
		if !id.Date.Equal(asOf) {
			fmt.Fprintf(&b, "Note: that snapshot is from the %s session, the quote from %s. If anything filled since, run fetch_snapshot first.\n", id.Date.Format("2006-01-02"), asOf.Format("2006-01-02"))
		}
		b.WriteString("\n")
	}
	req := report.TicketRequest{Account: in.Account, Kind: in.Kind, Option: opt.Instrument.ID, Limit: in.Limit, Qty: in.Qty}
	code, err := report.TicketOption(&b, bc.Cfg, bc.Rules, at, asOf, req, opt, quoteAt, in.JSON)
	return &b, code, snapID, shas, err
}

// ticketContract runs ticketByContract and records the run, with the stored
// responses it read as the run's inputs.
func (s *tools) ticketContract(ctx context.Context, in TicketIn) (*mcp.CallToolResult, Result, error) {
	bc, err := s.bk(in.Book)
	if err != nil {
		return nil, Result{}, err
	}
	b, code, snapID, inputs, err := s.ticketByContract(ctx, in)
	if err != nil {
		return nil, Result{}, err
	}
	id, err := store.RecordRun(ctx, s.d.DB, store.Run{
		Tool: "ticket", Args: in, SnapshotID: snapID, Book: bc.Name, RulesSHA: bc.RulesSHA, BookSHA: bc.BookSHA, AccountsSHA: s.d.AccountsSHA,
		Commit: s.d.Commit, Output: b.Bytes(), ExitCode: code, RanAt: s.d.Now(), Inputs: inputs,
	})
	if err != nil {
		return nil, Result{}, err
	}
	return &mcp.CallToolResult{Content: text(b)}, Result{ExitCode: code, RunID: id}, nil
}

// storeRawAlpaca stores an Alpaca response verbatim and reads it back; the
// ticket is computed from what was stored.
func (s *tools) storeRawAlpaca(ctx context.Context, r *alpacapull.Response) (fetched, []byte, error) {
	args, _ := json.Marshal(r.Args())
	sha, _, err := store.StoreRaw(ctx, s.d.DB, store.Raw{Call: r.Call(), Args: string(args), Body: r.Body, FetchedAt: r.FetchedAt, Source: AlpacaSource})
	if err != nil {
		return fetched{}, nil, err
	}
	body, err := store.RawBody(ctx, s.d.DB, sha)
	return fetched{tool: r.Call(), sha: sha, at: r.FetchedAt}, body, err
}

// resolveContractAlpaca finds the contract's instrument and quote through
// Alpaca's paper API: the contract record (it carries open interest) and the
// data API's option snapshot (the quote). Both are stored verbatim; the quote
// is last, so the ticket's quote time is the quote fetch's.
func (s *tools) resolveContractAlpaca(ctx context.Context, c occ.Contract) (*snapshot.Option, []fetched, error) {
	cl, _, err := s.alpacaClient(ctx)
	if err != nil {
		return nil, nil, err
	}
	dec, err := broker.Lookup(alpacaBroker)
	if err != nil {
		return nil, nil, err
	}
	sym := strings.ReplaceAll(c.String(), " ", "") // Alpaca writes the OCC symbol unpadded
	var got []fetched

	r, err := cl.OptionContracts(ctx, []string{sym})
	if err != nil {
		return nil, nil, err
	}
	f, body, err := s.storeRawAlpaca(ctx, r)
	if err != nil {
		return nil, nil, err
	}
	got = append(got, f)
	is, err := dec.Instruments(r.Call(), body)
	if err != nil {
		return nil, nil, err
	}
	if len(is) != 1 || is[0].ID != sym {
		return nil, nil, fmt.Errorf("%s: Alpaca returned %d contracts for this symbol; want exactly one", c, len(is))
	}

	r, err = cl.OptionSnapshots(ctx, []string{sym})
	if err != nil {
		return nil, nil, err
	}
	f, body, err = s.storeRawAlpaca(ctx, r)
	if err != nil {
		return nil, nil, err
	}
	got = append(got, f)
	qs, err := dec.OptionQuotes(r.Call(), body)
	if err != nil {
		return nil, nil, err
	}
	if len(qs) != 1 || qs[0].InstrumentID != sym {
		return nil, nil, fmt.Errorf("%s: Alpaca's option snapshot didn't return exactly this contract's quote", c)
	}
	o := &snapshot.Option{Quote: qs[0], Instrument: is[0]}
	o.JoinOpenInterest()
	return o, got, nil
}
