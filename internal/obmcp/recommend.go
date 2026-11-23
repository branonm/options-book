package obmcp

// recommend: covered-call strikes and expirations for one underlying. The
// server pulls the chain, the call instruments in a DTE window and their
// quotes from Robinhood, stores every response verbatim, reads them back, and
// runs each contract through the same ticket checks (wheel.Book.Recommend).
// Positions and blocks come from the given snapshot or the latest sealed one.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shopspring/decimal"

	"github.com/branonm/options-book/internal/broker/robinhood"
	"github.com/branonm/options-book/internal/report"
	"github.com/branonm/options-book/internal/rhmcp"
	"github.com/branonm/options-book/internal/snapshot"
	"github.com/branonm/options-book/internal/store"
	"github.com/branonm/options-book/internal/strategy/wheel"
)

// Defaults for the fetch window. They bound how much the server pulls; every
// rule is still checked by the ticket.
const (
	recMinDTE       = 14
	recMaxDTE       = 240
	recStrikeSpan   = "1.30" // strikes from spot up to spot × this
	recQuoteBatch   = 20     // get_option_quotes omits closes above 20 ids
	recMaxContracts = 400
)

type RecommendIn struct {
	Book     string `json:"book,omitempty" jsonschema:"the book to run, e.g. ira-wheel; default ira-wheel"`
	Snapshot string `json:"snapshot,omitempty" jsonschema:"snapshot id for positions and blocks; default the latest sealed"`
	Symbol   string `json:"symbol" jsonschema:"the underlying, e.g. EEM"`
	Account  string `json:"account,omitempty" jsonschema:"one account, e.g. roth; default every account with free lots of symbol"`
	MinDTE   int    `json:"min_dte,omitempty" jsonschema:"nearest expiration to consider, in calendar days; default 14"`
	MaxDTE   int    `json:"max_dte,omitempty" jsonschema:"furthest expiration to consider, in calendar days; default 240"`
	JSON     bool   `json:"json,omitempty" jsonschema:"machine-readable JSON instead of text"`
}

func registerRecommend(server *mcp.Server, s *tools) {
	mcp.AddTool(server, &mcp.Tool{Name: "recommend",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: boolp(false), IdempotentHint: false, OpenWorldHint: boolp(true)},
		Description: "Covered-call strikes and expirations for one underlying, per account with free lots: the server pulls the chain, " +
			"the calls in a DTE window and their quotes from Robinhood, stores each response verbatim, and runs every contract through the " +
			"ticket checks (open interest, quote window, Rule 11 breakeven and graded clearance, delta band). Lists every contract that passes " +
			"with its limit (midpoint rounded down to the tick, never below the bid), credit, breakeven after, clearance and call-away payoff; " +
			"marks the best per expiry and a pick on the emptiest ladder rung (Rule 1). exit_code 1 means nothing passed. Places no order."},
		expose(s.recommend))
}

func (s *tools) recommend(ctx context.Context, _ *mcp.CallToolRequest, in RecommendIn) (*mcp.CallToolResult, Result, error) {
	bc, err := s.bk(in.Book)
	if err != nil {
		return nil, Result{}, err
	}
	if bc.Cfg.Source() == alpacaBroker {
		return nil, Result{}, fmt.Errorf("recommend pulls from Robinhood; book %s is an Alpaca book", bc.Name)
	}
	sym := strings.ToUpper(strings.TrimSpace(in.Symbol))
	if sym == "" {
		return nil, Result{}, fmt.Errorf("recommend needs a symbol")
	}
	minD, maxD := in.MinDTE, in.MaxDTE
	if minD <= 0 {
		minD = recMinDTE
	}
	if maxD <= 0 {
		maxD = recMaxDTE
	}
	if maxD < minD {
		return nil, Result{}, fmt.Errorf("max_dte %d is under min_dte %d", maxD, minD)
	}

	snapID, sealedAt := in.Snapshot, ""
	if snapID == "" {
		if snapID, sealedAt, err = store.LatestSealed(ctx, s.d.DB); err != nil {
			return nil, Result{}, err
		}
	}
	src, err := s.source(ctx)
	if err != nil {
		return nil, Result{}, err
	}
	at, err := s.at(src, snapID)
	if err != nil {
		return nil, Result{}, err
	}
	id, err := snapshot.ParseID(snapID)
	if err != nil {
		return nil, Result{}, err
	}

	// Spot and floors from the snapshot, to bound the strikes fetched.
	_, bk, err := report.Assemble(bc.Cfg, at, id.Date)
	if err != nil {
		return nil, Result{}, err
	}
	lo, hi, err := strikeWindow(bk, sym, in.Account)
	if err != nil {
		return nil, Result{}, err
	}

	ny, err := time.LoadLocation(bc.Rules.QuoteWindow.Timezone)
	if err != nil {
		return nil, Result{}, fmt.Errorf("quote window timezone: %w", err)
	}
	rc, _, err := s.robinhood(ctx)
	if err != nil {
		return nil, Result{}, err
	}
	opts, got, err := s.fetchCalls(ctx, rc, sym, s.d.Now().In(ny), minD, maxD, lo, hi)
	if err != nil {
		return nil, Result{}, err
	}
	quoteAt := got[len(got)-1].at
	lt := quoteAt.In(ny)
	asOf := time.Date(lt.Year(), lt.Month(), lt.Day(), 0, 0, 0, 0, time.UTC)

	var b bytes.Buffer
	var shas []string
	for _, f := range got {
		shas = append(shas, f.sha)
	}
	if !in.JSON {
		fmt.Fprintf(&b, "%s calls %d–%d DTE, strikes $%s–$%s: %d contracts quoted by Robinhood at %s (fetched by this server; %d responses stored verbatim).\n",
			sym, minD, maxD, lo.StringFixed(2), hi.StringFixed(2), len(opts), lt.Format("Mon 2006-01-02 15:04:05 MST"), len(got))
		if sealedAt != "" {
			fmt.Fprintf(&b, "Positions and blocks: snapshot %s, the latest sealed (%s).\n", snapID, sealedAt)
		} else {
			fmt.Fprintf(&b, "Positions and blocks: snapshot %s.\n", snapID)
		}
		if !id.Date.Equal(asOf) {
			fmt.Fprintf(&b, "Note: that snapshot is from the %s session, the quotes from %s. If anything filled since, run fetch_snapshot first.\n", id.Date.Format("2006-01-02"), asOf.Format("2006-01-02"))
		}
		b.WriteString("\n")
	}
	req := wheel.RecommendRequest{Symbol: sym, Account: in.Account, Options: opts, FetchedAt: quoteAt}
	code, err := report.Recommend(&b, bc.Cfg, bc.Rules, at, asOf, req, in.JSON)
	if err != nil {
		return nil, Result{}, err
	}
	runID, err := store.RecordRun(ctx, s.d.DB, store.Run{
		Tool: "recommend", Args: in, SnapshotID: snapID, Book: bc.Name, RulesSHA: bc.RulesSHA, BookSHA: bc.BookSHA, AccountsSHA: s.d.AccountsSHA,
		Commit: s.d.Commit, Output: b.Bytes(), ExitCode: code, RanAt: s.d.Now(), Inputs: shas,
	})
	if err != nil {
		return nil, Result{}, err
	}
	return &mcp.CallToolResult{Content: text(&b)}, Result{ExitCode: code, RunID: runID}, nil
}

// strikeWindow bounds the strikes worth quoting: from the lowest of spot and
// the breakevens of the blocks in scope (an ITM call fails the delta band
// anyway) up to the highest of those × recStrikeSpan.
func strikeWindow(bk *wheel.Book, sym, account string) (lo, hi decimal.Decimal, err error) {
	var points []decimal.Decimal
	for _, a := range bk.Accounts() {
		if account != "" && a.Name != account {
			continue
		}
		for _, b := range a.Blocks {
			if b.Symbol == sym {
				points = append(points, b.Price, b.Breakeven)
			}
		}
	}
	lo, hi, ok := windowOf(points)
	if !ok {
		if account != "" {
			return lo, hi, fmt.Errorf("%s holds no %s in the snapshot", account, sym)
		}
		return lo, hi, fmt.Errorf("no account in the book holds %s in the snapshot", sym)
	}
	return lo, hi, nil
}

type chainsBody struct {
	Data struct {
		Chains []struct {
			Symbol          string   `json:"symbol"`
			ExpirationDates []string `json:"expiration_dates"`
		} `json:"chains"`
	} `json:"data"`
}

// fetchCalls pulls the chain, the call instruments at expirations in the DTE
// window, and their quotes, storing each response and decoding what was stored.
func (s *tools) fetchCalls(ctx context.Context, rc *rhmcp.Client, sym string, now time.Time, minD, maxD int, lo, hi decimal.Decimal) ([]*snapshot.Option, []fetched, error) {
	var got []fetched

	// 1. The chain's expirations.
	var chainArgs map[string]any
	for _, name := range []string{"chain_symbol", "symbol", "symbols", "equity_symbol", "underlying_symbol"} {
		v, ok, err := arg(ctx, rc, "get_option_chains", name, sym)
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
	f, body, err := s.storeRaw(ctx, r)
	if err != nil {
		return nil, nil, err
	}
	got = append(got, f)
	var cb chainsBody
	if err := json.Unmarshal(body, &cb); err != nil {
		return nil, nil, fmt.Errorf("get_option_chains: %w", err)
	}
	exps := expirationsInWindow(cb, sym, now, minD, maxD)
	seen := map[string]bool{}
	for _, e := range exps {
		seen[e] = true
	}
	if len(exps) == 0 {
		return nil, nil, fmt.Errorf("%s has no expirations %d–%d days out", sym, minD, maxD)
	}

	// 2. The call instruments at those expirations, every page.
	instArgs := map[string]any{}
	if v, ok, err := arg(ctx, rc, "get_option_instruments", "chain_symbol", sym); err != nil {
		return nil, nil, err
	} else if ok {
		instArgs["chain_symbol"] = v
	} else {
		params, _ := rc.Params(ctx, "get_option_instruments")
		return nil, nil, fmt.Errorf("get_option_instruments doesn't take chain_symbol (it takes %v)", params)
	}
	if p, ok, err := rc.Property(ctx, "get_option_instruments", "expiration_dates"); err != nil {
		return nil, nil, err
	} else if ok {
		if p["type"] == "array" {
			instArgs["expiration_dates"] = exps
		} else {
			instArgs["expiration_dates"] = strings.Join(exps, ",")
		}
	}
	if v, ok, err := arg(ctx, rc, "get_option_instruments", "type", "call"); err != nil {
		return nil, nil, err
	} else if ok {
		instArgs["type"] = v
	}
	pages, err := rc.Pages(ctx, "get_option_instruments", instArgs)
	if err != nil {
		return nil, nil, err
	}
	insts := map[string]*snapshot.Option{}
	var ids []string
	for _, p := range pages {
		f, body, err := s.storeRaw(ctx, p)
		if err != nil {
			return nil, nil, err
		}
		got = append(got, f)
		page, err := robinhood.InstrumentsPage("get_option_instruments", body)
		if err != nil {
			return nil, nil, err
		}
		for _, in := range page {
			strike, err := decimal.NewFromString(in.Strike.Text)
			if err != nil || in.Symbol != sym || in.Right != "call" || !seen[in.Expiry] || strike.LessThan(lo) || strike.GreaterThan(hi) {
				continue
			}
			if _, dup := insts[in.ID]; !dup {
				insts[in.ID] = &snapshot.Option{Instrument: in}
				ids = append(ids, in.ID)
			}
		}
	}
	if len(ids) == 0 {
		return nil, nil, fmt.Errorf("%s: no calls between $%s and $%s at %s", sym, lo, hi, strings.Join(exps, ", "))
	}
	if len(ids) > recMaxContracts {
		return nil, nil, fmt.Errorf("%s: %d contracts in the window, over %d; narrow min_dte/max_dte", sym, len(ids), recMaxContracts)
	}

	// 3. Their quotes, in batches.
	dec, err := snapshot.Decoder()
	if err != nil {
		return nil, nil, err
	}
	for i := 0; i < len(ids); i += recQuoteBatch {
		batch := ids[i:min(i+recQuoteBatch, len(ids))]
		r, err := rc.Call(ctx, "get_option_quotes", map[string]any{"instrument_ids": batch})
		if err != nil {
			return nil, nil, err
		}
		f, body, err := s.storeRaw(ctx, r)
		if err != nil {
			return nil, nil, err
		}
		got = append(got, f)
		qs, err := dec.OptionQuotes("get_option_quotes", body)
		if err != nil {
			return nil, nil, err
		}
		for _, q := range qs {
			if o, ok := insts[q.InstrumentID]; ok {
				o.Quote = q
			}
		}
	}
	var out []*snapshot.Option
	for _, id := range ids {
		if o := insts[id]; o.Quote.InstrumentID != "" {
			out = append(out, o)
		}
	}
	if len(out) == 0 {
		return nil, nil, fmt.Errorf("%s: get_option_quotes returned no quotes", sym)
	}
	return out, got, nil
}

// expirationsInWindow lists the chain's expirations for sym that are minD..maxD
// calendar days out, counting from now'"'"'s date in now'"'"'s own zone (the market's,
// not UTC's, so an evening quote doesn'"'"'t shift the window a day).
func expirationsInWindow(cb chainsBody, sym string, now time.Time, minD, maxD int) []string {
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	seen := map[string]bool{}
	var exps []string
	for _, c := range cb.Data.Chains {
		if c.Symbol != sym {
			continue
		}
		for _, e := range c.ExpirationDates {
			d, err := time.Parse("2006-01-02", e)
			if err != nil || seen[e] {
				continue
			}
			if dte := int(d.Sub(today).Hours() / 24); dte >= minD && dte <= maxD {
				seen[e] = true
				exps = append(exps, e)
			}
		}
	}
	return exps
}

// windowOf bounds the strikes from the prices given (spot and breakevens): from
// the lowest up to the highest × recStrikeSpan, whole dollars outward.
func windowOf(points []decimal.Decimal) (lo, hi decimal.Decimal, ok bool) {
	span, _ := decimal.NewFromString(recStrikeSpan)
	for _, p := range points {
		if !p.IsPositive() {
			continue
		}
		if !ok || p.LessThan(lo) {
			lo = p
		}
		if top := p.Mul(span); !ok || top.GreaterThan(hi) {
			hi = top
		}
		ok = true
	}
	return lo.Floor(), hi.Ceil(), ok
}
