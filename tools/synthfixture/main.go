// Command synthfixture writes the synthetic repo root under testdata/synthetic:
// a fictional two-account wheel book (AAA, BBB, CCC; accounts 0001 and 0002)
// shaped exactly like the real data and small enough that every figure can be
// checked by hand. Nothing in it comes from a real account.
//
//	go build -o bin/ob ./cmd/ob && go run ./tools/synthfixture -ob bin/ob -out testdata/synthetic
//
// It writes the raw Robinhood-shaped responses, then runs the repo's own
// commands (history add, snapshot, expirations) to build the order log and seal
// each snapshot, so the files are made the way real ones are. The hand-computed
// figures every test pins are in testdata/synthetic/README.md.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/branonm/options-book/internal/layout"
)

var (
	obBin = flag.String("ob", "bin/ob", "the built ob binary")
	out   = flag.String("out", "testdata/synthetic", "output root")
)

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "synthfixture:", err)
		os.Exit(1)
	}
}

func write(rel string, b []byte) {
	p := filepath.Join(*out, rel)
	must(os.MkdirAll(filepath.Dir(p), 0o755))
	must(os.WriteFile(p, b, 0o644))
}

func writeJSON(rel string, v any) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	must(enc.Encode(v))
	write(rel, b.Bytes())
}

// run executes ob from the output root, as a check-in does from the repo root.
func run(args ...string) string {
	abs, err := filepath.Abs(*obBin)
	must(err)
	cmd := exec.Command(abs, args...)
	cmd.Dir = *out
	var o bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &o
	if err := cmd.Run(); err != nil {
		must(fmt.Errorf("ob %s: %v\n%s", strings.Join(args, " "), err, o.String()))
	}
	return o.String()
}

// ---- ids ----

func oid(n int) string { return fmt.Sprintf("a0000000-0000-4000-8000-%012d", n) } // option instrument
// uid is an order, chain, leg or execution id. The first eight characters differ
// per id, because a lookup page is saved as lookup_<first eight of the order id>.
func uid(n int) string { return fmt.Sprintf("b%07d-0000-4000-8000-%012d", n, n) }

// ---- order records ----

type exec_ struct {
	ID             string `json:"id"`
	Price          string `json:"price"`
	Quantity       string `json:"quantity"`
	SettlementDate string `json:"settlement_date"`
	TradeDate      string `json:"trade_date"`
	Timestamp      string `json:"timestamp"`
}

type leg struct {
	ID             string  `json:"id"`
	OptionID       string  `json:"option_id"`
	Side           string  `json:"side"`
	PositionEffect string  `json:"position_effect"`
	RatioQuantity  int     `json:"ratio_quantity"`
	Expiration     string  `json:"expiration_date"`
	Strike         string  `json:"strike_price"`
	OptionType     string  `json:"option_type"`
	Executions     []exec_ `json:"executions,omitempty"`
}

type order struct {
	ID               string  `json:"id"`
	ChainID          string  `json:"chain_id"`
	Symbol           string  `json:"chain_symbol"`
	State            string  `json:"state"`
	Type             string  `json:"type"`
	Trigger          string  `json:"trigger"`
	Direction        string  `json:"direction"`
	Quantity         string  `json:"quantity"`
	ProcessedQty     string  `json:"processed_quantity"`
	PendingQty       string  `json:"pending_quantity"`
	CanceledQty      string  `json:"canceled_quantity"`
	Price            string  `json:"price"`
	StopPrice        *string `json:"stop_price"`
	Premium          string  `json:"premium"`
	ProcessedPremium string  `json:"processed_premium"`
	Multiplier       string  `json:"trade_value_multiplier"`
	TimeInForce      string  `json:"time_in_force"`
	MarketHours      string  `json:"market_hours"`
	OpeningStrategy  *string `json:"opening_strategy"`
	ClosingStrategy  *string `json:"closing_strategy"`
	PlacedAgent      string  `json:"placed_agent"`
	AgentInitiated   bool    `json:"agent_initiated"`
	CreatedAt        string  `json:"created_at"`
	UpdatedAt        string  `json:"updated_at"`
	LastTransaction  *string `json:"last_transaction_at"`
	IsReplaceable    bool    `json:"is_replaceable"`
	Legs             []leg   `json:"legs"`
}

// spec is one order in the scenario, in whole dollars and contracts.
type spec struct {
	n          int    // order number (ids derive from it)
	sym        string // chain_symbol
	optN       int    // option instrument number
	side       string // sell (open) | buy (close)
	qty        int
	cents      int    // limit price per share, in cents
	strike     string // "55.0000"
	expiry     string
	created    string // RFC3339
	state      string // filled | confirmed | cancelled
	filledAt   string // execution/update time when filled or cancelled
	tif        string // gtc | gfd
	optionType string
}

func sptr(s string) *string { return &s }

func (s spec) build() order {
	open := s.side == "sell"
	dir, effect := "credit", "open"
	if !open {
		dir, effect = "debit", "close"
	}
	strat := "short_call"
	if s.optionType == "put" {
		strat = "short_put"
	}
	o := order{
		ID: uid(s.n), ChainID: uid(s.n + 5000), Symbol: s.sym, State: s.state, Type: "limit", Trigger: "immediate",
		Direction: dir, Quantity: fmt.Sprintf("%d.00000", s.qty), CanceledQty: "0.00000", Price: fmt.Sprintf("%d.%02d000000", s.cents/100, s.cents%100),
		Premium: fmt.Sprintf("%d.00000000", s.cents*s.qty), Multiplier: "100.0000", TimeInForce: s.tif, MarketHours: "regular_hours",
		PlacedAgent: "user", CreatedAt: s.created, UpdatedAt: s.created, IsReplaceable: s.state == "confirmed",
	}
	if open {
		o.OpeningStrategy = sptr(strat)
	} else {
		o.ClosingStrategy = sptr(strat)
	}
	l := leg{ID: uid(s.n + 9000), OptionID: oid(s.optN), Side: s.side, PositionEffect: effect, RatioQuantity: 1, Expiration: s.expiry, Strike: s.strike, OptionType: s.optionType}
	switch s.state {
	case "filled":
		o.ProcessedQty, o.PendingQty, o.ProcessedPremium = o.Quantity, "0.00000", fmt.Sprintf("%d", s.cents*s.qty)
		o.UpdatedAt = s.filledAt
		l.Executions = []exec_{{ID: uid(s.n + 13000), Price: o.Price, Quantity: o.Quantity, SettlementDate: s.filledAt[:10], Timestamp: strings.TrimSuffix(s.filledAt, "Z") + ".000000Z"}}
	case "cancelled":
		o.ProcessedQty, o.PendingQty, o.CanceledQty, o.ProcessedPremium = "0.00000", "0.00000", o.Quantity, "0"
		o.UpdatedAt = s.filledAt
	default: // confirmed
		o.ProcessedQty, o.PendingQty, o.ProcessedPremium = "0.00000", o.Quantity, "0"
	}
	o.Legs = []leg{l}
	return o
}

type orders struct {
	Data struct {
		Orders []order `json:"orders"`
	} `json:"data"`
}

// lookupPage is the name fetch_snapshot gives a by-id lookup page.
func lookupPage(s spec) string { return "lookup_" + uid(s.n)[:8] }

func page(specs ...spec) orders {
	var p orders
	p.Data.Orders = []order{}
	for _, s := range specs {
		p.Data.Orders = append(p.Data.Orders, s.build())
	}
	return p
}

// ---- the scenario ----
//
// Roth (0001): AAA 300 sh basis 50.00, BBB 100 sh basis 80.00.
// Trad (0002): CCC 200 sh basis 40.00. Campaign starts 2026-01-05.

var (
	// roth
	r1Open  = spec{n: 1, sym: "AAA", optN: 1, side: "sell", qty: 3, cents: 100, strike: "55.0000", expiry: "2026-01-23", created: "2026-01-09T15:00:00Z", state: "filled", filledAt: "2026-01-09T15:00:02Z", tif: "gfd", optionType: "call"}
	r1Close = spec{n: 2, sym: "AAA", optN: 1, side: "buy", qty: 3, cents: 50, strike: "55.0000", expiry: "2026-01-23", created: "2026-01-09T15:05:00Z", state: "filled", filledAt: "2026-01-16T14:30:00Z", tif: "gtc", optionType: "call"}
	r2Open  = spec{n: 3, sym: "AAA", optN: 2, side: "sell", qty: 3, cents: 150, strike: "55.0000", expiry: "2026-03-20", created: "2026-02-02T15:10:00Z", state: "filled", filledAt: "2026-02-02T15:10:02Z", tif: "gfd", optionType: "call"}
	r2Close = spec{n: 4, sym: "AAA", optN: 2, side: "buy", qty: 3, cents: 75, strike: "55.0000", expiry: "2026-03-20", created: "2026-02-02T15:15:00Z", state: "confirmed", tif: "gtc", optionType: "call"}
	r3Open  = spec{n: 5, sym: "BBB", optN: 3, side: "sell", qty: 1, cents: 200, strike: "85.0000", expiry: "2026-02-20", created: "2026-01-26T15:00:00Z", state: "filled", filledAt: "2026-01-26T15:00:02Z", tif: "gfd", optionType: "call"}
	// the closer at snapshot A is off Rule 10 ($0.90, not $0.80): a defect
	r3Bad    = spec{n: 6, sym: "BBB", optN: 3, side: "buy", qty: 1, cents: 110, strike: "85.0000", expiry: "2026-02-20", created: "2026-01-26T15:05:00Z", state: "confirmed", tif: "gtc", optionType: "call"}
	r3BadCxl = func() spec { s := r3Bad; s.state, s.filledAt = "cancelled", "2026-02-09T15:00:00Z"; return s }()
	r3Good   = spec{n: 7, sym: "BBB", optN: 3, side: "buy", qty: 1, cents: 100, strike: "85.0000", expiry: "2026-02-20", created: "2026-02-09T15:01:00Z", state: "filled", filledAt: "2026-02-10T16:20:00Z", tif: "gtc", optionType: "call"}
	// trad
	t1Open  = spec{n: 11, sym: "CCC", optN: 11, side: "sell", qty: 2, cents: 50, strike: "44.0000", expiry: "2026-01-30", created: "2026-01-23T15:00:00Z", state: "filled", filledAt: "2026-01-23T15:00:02Z", tif: "gfd", optionType: "call"}
	t2Open  = spec{n: 12, sym: "CCC", optN: 12, side: "sell", qty: 2, cents: 80, strike: "44.0000", expiry: "2026-02-20", created: "2026-01-30T15:30:00Z", state: "filled", filledAt: "2026-01-30T15:30:02Z", tif: "gfd", optionType: "call"}
	t2Close = spec{n: 13, sym: "CCC", optN: 12, side: "buy", qty: 2, cents: 40, strike: "44.0000", expiry: "2026-02-20", created: "2026-01-30T15:35:00Z", state: "confirmed", tif: "gtc", optionType: "call"}
)

func main() {
	flag.Parse()
	must(os.RemoveAll(*out))
	must(os.MkdirAll(*out, 0o755))

	// config and book
	write(layout.AccountsConfig, []byte(`{
  "_doc": "SYNTHETIC. Two fictional accounts; nothing here is real.",
  "accounts": [
    {"name": "roth", "label": "Roth IRA",        "last4": "0001", "source": "robinhood", "default_book": "ira-wheel", "modes": ["browser", "manual"]},
    {"name": "trad", "label": "Traditional IRA", "last4": "0002", "source": "robinhood", "default_book": "ira-wheel", "modes": ["browser", "manual"]},
    {"name": "taxable", "label": "Taxable", "last4": "0003", "source": "robinhood", "default_book": null, "modes": ["browser", "manual"]}
  ]
}
`))
	write(layout.BookFile(layout.DefaultBook), []byte(`{
  "strategy": "wheel",
  "doc": "wheel-book.md",
  "campaign_start": "2026-01-05",
  "accounts": [
    {"name": "roth", "pool": "roth",        "expected_symbols": ["AAA", "BBB"]},
    {"name": "trad", "pool": "traditional", "expected_symbols": ["CCC"]}
  ]
}
`))
	write(layout.RulesFile(layout.DefaultBook), []byte(syntheticRules))
	write("wheel-book.md", []byte("# Wheel Book — SYNTHETIC (Roth ••0001, Traditional ••0002)\n\nFictional data for the test suite.\n\n<!-- BEGIN GENERATED: wheel book -->\n<!-- END GENERATED -->\n"))

	snapshotA()
	snapshotB()
	run("book", "-snapshot", layout.Snapshot("2026-02-13"), "-into", "wheel-book.md")
	write("README.md", []byte(readme))
	fmt.Println("wrote", *out)
}

func positions(rel string, eq [][3]string, opts []map[string]string) {
	type ep struct {
		Symbol string `json:"symbol"`
		Qty    string `json:"quantity"`
		Avg    string `json:"average_buy_price"`
		Type   string `json:"type"`
	}
	var e struct {
		Data struct {
			Positions []ep `json:"positions"`
		} `json:"data"`
	}
	e.Data.Positions = []ep{}
	for _, p := range eq {
		e.Data.Positions = append(e.Data.Positions, ep{p[0], p[1], p[2], "long"})
	}
	writeJSON(rel+"/equity_positions.json", e)

	type op struct {
		OptionID   string `json:"option_id"`
		Symbol     string `json:"chain_symbol"`
		Type       string `json:"type"`
		Quantity   string `json:"quantity"`
		AvgPrice   string `json:"average_price"`
		Expiration string `json:"expiration_date"`
		Pending    string `json:"pending_buy_quantity"`
	}
	var o struct {
		Data struct {
			Positions []op `json:"positions"`
		} `json:"data"`
	}
	o.Data.Positions = []op{}
	for _, p := range opts {
		o.Data.Positions = append(o.Data.Positions, op{p["id"], p["sym"], "short", p["qty"], p["avg"], p["exp"], p["pending"]})
	}
	writeJSON(rel+"/option_positions.json", o)
}

func portfolio(rel, total, equity, options, cash string) {
	writeJSON(rel+"/portfolio.json", map[string]any{"data": map[string]any{
		"total_value": total, "equity_value": equity, "options_value": options, "cash": cash, "currency": "USD",
		"buying_power": map[string]any{"buying_power": cash, "unleveraged_buying_power": cash, "display_currency": "USD"},
	}})
}

func quotes(rel, when string, equity [][2]string, options [][]string) {
	var eq strings.Builder
	eq.WriteString("{\"data\":{\"results\":[\n")
	for i, q := range equity {
		fmt.Fprintf(&eq, "{\"quote\":{\"symbol\":%q,\"last_trade_price\":%q,\"venue_last_trade_time\":%q,\"state\":\"active\"}}", q[0], q[1], when)
		if i < len(equity)-1 {
			eq.WriteString(",")
		}
		eq.WriteString("\n")
	}
	eq.WriteString("]}}\n")
	write(rel+"/equity_quotes.json", []byte(eq.String()))

	var oq strings.Builder
	oq.WriteString("{\"data\":{\"results\":[\n")
	for i, q := range options { // id, bid, ask, mark, delta
		fmt.Fprintf(&oq, "{\"quote\":{\"instrument_id\":%q,\"bid_price\":%q,\"ask_price\":%q,\"mark_price\":%q,\"implied_volatility\":\"0.250000\",\"delta\":%q,\"open_interest\":5000,\"volume\":100,\"updated_at\":%q}}", q[0], q[1], q[2], q[3], q[4], when)
		if i < len(options)-1 {
			oq.WriteString(",")
		}
		oq.WriteString("\n")
	}
	oq.WriteString("]}}\n")
	write(rel+"/option_quotes.json", []byte(oq.String()))
}

// expected writes the hand-computed figures for a snapshot (see README.md): they
// are worked out from the scenario, never copied from tool output.
func expected(rel, asof string, legs int, rothRealized, rothOpen, rothBlocks, tradRealized, tradOpen, tradBlock, checkin string) {
	write(rel+"/expected.json", []byte(fmt.Sprintf(`{
  "asof": %q,
  "source": "SYNTHETIC. Worked out by hand from testdata/synthetic/README.md; not tool output.",
  "accounts": {
    "roth": {"realized": %q, "open_credit": %q, "basis_credit": "0", "blocks": {%s}},
    "trad": {"realized": %q, "open_credit": %q, "basis_credit": "0", "blocks": {%s}}
  },
  "combined": {"realized_legs": %d},
  "checkin": %s
}
`, asof, rothRealized, rothOpen, rothBlocks, tradRealized, tradOpen, tradBlock, legs, checkin)))
}

// candidate saves a contract to re-cover the uncovered BBB block with: a BBB
// $85 call expiring 2026-03-20 (the ticket tests' covered call). Its quote is
// stamped inside the 2026-02-13 session.
func candidate(id, date string) {
	const instrument = "a0000000-0000-4000-8000-000000000021"
	dir := id + "/" + layout.OptionFolder(instrument)
	write(dir+"/"+layout.InstrumentFile, []byte(`{"data":{"instruments":[{"id":"`+instrument+`","chain_id":"b0000000-0000-4000-8000-000000000021","chain_symbol":"BBB","underlying_type":"equity","expiration_date":"2026-03-20","strike_price":"85.0000","type":"call","state":"active","tradability":"tradable","trade_value_multiplier":"100.0000","min_ticks":{"above_tick":"0.05","below_tick":"0.01","cutoff_price":"3.00"}}]}}
`))
	write(dir+"/"+layout.QuoteFile, []byte(`{"data":{"results":[{"quote":{"instrument_id":"`+instrument+`","bid_price":"1.200000","ask_price":"1.300000","mark_price":"1.250000","implied_volatility":"0.300000","delta":"0.280000","open_interest":5000,"volume":100,"updated_at":"`+date+`T20:59:00.000000000Z"}}]}}
`))
}

func provenance(rel, id string) {
	write(rel+"/PROVENANCE.md", []byte(fmt.Sprintf("# Snapshot %s — provenance\n\n**SYNTHETIC.** Written by `tools/synthfixture` in the shape of Robinhood's MCP responses. Nothing here came from a real account.\n", id)))
}

// snapshotA: Friday 2026-02-06, after close.
func snapshotA() {
	id := layout.Snapshot("2026-02-06")
	// full pulls (first pull: the log is empty)
	writeJSON(id+"/roth/orders_pull/full_p1.json", page(r1Open, r1Close, r2Open, r2Close, r3Open, r3Bad))
	writeJSON(id+"/trad/orders_pull/full_p1.json", page(t1Open, t2Open, t2Close))

	positions(id+"/roth", [][3]string{{"AAA", "300.000000", "50.000000"}, {"BBB", "100.000000", "80.000000"}}, []map[string]string{
		{"id": oid(2), "sym": "AAA", "qty": "3.0000", "avg": "-150.0000", "exp": "2026-03-20", "pending": "3.0000"},
		{"id": oid(3), "sym": "BBB", "qty": "1.0000", "avg": "-200.0000", "exp": "2026-02-20", "pending": "1.0000"},
	})
	positions(id+"/trad", [][3]string{{"CCC", "200.000000", "40.000000"}}, []map[string]string{
		{"id": oid(12), "sym": "CCC", "qty": "2.0000", "avg": "-80.0000", "exp": "2026-02-20", "pending": "2.0000"},
	})
	// roth: equity 300*52 + 100*82 = 23800; options -(0.80*3 + 1.50*1)*100 = -390; cash 5000
	portfolio(id+"/roth", "28410", "23800", "-390", "5000")
	// trad: equity 200*41 = 8200; options -(0.40*2)*100 = -80; cash 1000
	portfolio(id+"/trad", "9120", "8200", "-80", "1000")
	quotes(id, "2026-02-06T21:00:00Z",
		[][2]string{{"AAA", "52.000000"}, {"BBB", "82.000000"}, {"CCC", "41.000000"}},
		[][]string{
			{oid(2), "0.780000", "0.820000", "0.800000", "0.250000"},
			{oid(3), "1.450000", "1.550000", "1.500000", "0.300000"},
			{oid(12), "0.380000", "0.420000", "0.400000", "0.200000"},
		})
	provenance(id, "2026-02-06")
	candidate(id, "2026-02-06")
	expected(id, "2026-02-06", 2, "150", "650", `"AAA": {"shares": 300, "premium": "600", "breakeven": "48.00"}, "BBB": {"shares": 100, "premium": "200", "breakeven": "78.00"}`, "100", "160", `"CCC": {"shares": 200, "premium": "260", "breakeven": "38.70"}`,
		`{"defects": [{"account": "roth", "check": "closer_price", "subject": "BBB $85c 2026-02-20", "expected": "$1.00", "actual": "$1.10"}], "alerts": []}`)

	// the expired CCC call: its outcome comes from the underlying's close on the expiry date
	write(layout.Bars("2026-02-06_ccc"), []byte(`{"_provenance": "SYNTHETIC. Shaped like get_equity_historicals(symbols=[CCC], interval=day, adjustment_type=none); only the expiration-day bar is kept.",
 "data":{"results":[
  {"symbol":"CCC","interval":"day","bounds":"regular","bars":[{"begins_at":"2026-01-30T00:00:00Z","open_price":"41.000000","close_price":"41.200000","high_price":"41.500000","low_price":"40.800000","volume":1000000,"session":"reg"}]}
]}}
`))

	run("history", "add", "-account", "roth", "-observed", "2026-02-06T22:00:00Z", "-full", "-pull", id+"/roth/orders_pull/full_p1.json", "-snapshot", id)
	run("history", "add", "-account", "trad", "-observed", "2026-02-06T22:00:00Z", "-full", "-pull", id+"/trad/orders_pull/full_p1.json", "-snapshot", id)
	run("expirations", "-snapshot", id, "-bars", layout.Bars("2026-02-06_ccc"))
	fmt.Print(run("snapshot", "-snapshot", id))
	sealedAt(id, "2026-02-06T22:05:00Z")
	golden(id, "2026-02-06")
}

// sealedAt sets the manifest's created_at to the session's own seal time. The
// seal command stamps the clock; a fixed time keeps the fixture repeatable and
// makes "the latest sealed snapshot" the later session, not a tie. The file
// hashes in the manifest don't cover created_at.
func sealedAt(id, when string) {
	p := filepath.Join(*out, id, layout.Manifest)
	b, err := os.ReadFile(p)
	must(err)
	re := regexp.MustCompile("\"created_at\": \"[^\"]*\"")
	if !re.Match(b) {
		must(fmt.Errorf("%s: no created_at", p))
	}
	must(os.WriteFile(p, re.ReplaceAll(b, []byte("\"created_at\": \""+when+"\"")), 0o644))
}

// golden writes the snapshot's generated wheel book beside it (a test golden).
func golden(id, date string) {
	write(layout.InSnapshot(date, layout.GeneratedBook), []byte(run("book", "-snapshot", id)))
}

// snapshotB: Friday 2026-02-13, after close. The off-rule BBB closer was
// cancelled and replaced with the Rule 10 price, and that closer filled.
func snapshotB() {
	id := layout.Snapshot("2026-02-13")
	// window pull: orders created since 2026-02-06 (the replacement closer)
	writeJSON(id+"/roth/orders_pull/window_p1.json", page(r3Good))
	// open pull: what is still confirmed
	writeJSON(id+"/roth/orders_pull/open_p1.json", page(r2Close))
	// by-id lookup: the cancelled off-rule closer was open in the log
	writeJSON(id+"/roth/orders_pull/"+lookupPage(r3Bad)+".json", page(r3BadCxl))
	writeJSON(id+"/trad/orders_pull/window_p1.json", page())
	writeJSON(id+"/trad/orders_pull/open_p1.json", page(t2Close))

	positions(id+"/roth", [][3]string{{"AAA", "300.000000", "50.000000"}, {"BBB", "100.000000", "80.000000"}}, []map[string]string{
		{"id": oid(2), "sym": "AAA", "qty": "3.0000", "avg": "-150.0000", "exp": "2026-03-20", "pending": "3.0000"},
	})
	positions(id+"/trad", [][3]string{{"CCC", "200.000000", "40.000000"}}, []map[string]string{
		{"id": oid(12), "sym": "CCC", "qty": "2.0000", "avg": "-80.0000", "exp": "2026-02-20", "pending": "2.0000"},
	})
	// roth: equity 300*53 + 100*83 = 24200; options -(0.70*3)*100 = -210; cash 5120 (BBB closer: -80 + 200 earlier)
	portfolio(id+"/roth", "29110", "24200", "-210", "5120")
	// trad: equity 200*42 = 8400; options -(0.30*2)*100 = -60; cash 1000
	portfolio(id+"/trad", "9340", "8400", "-60", "1000")
	quotes(id, "2026-02-13T21:00:00Z",
		[][2]string{{"AAA", "53.000000"}, {"BBB", "83.000000"}, {"CCC", "42.000000"}},
		[][]string{
			{oid(2), "0.680000", "0.720000", "0.700000", "0.250000"},
			{oid(12), "0.280000", "0.320000", "0.300000", "0.200000"},
		})
	provenance(id, "2026-02-13")
	candidate(id, "2026-02-13")
	expected(id, "2026-02-13", 3, "250", "450", `"AAA": {"shares": 300, "premium": "600", "breakeven": "48.00"}, "BBB": {"shares": 100, "premium": "100", "breakeven": "79.00"}`, "100", "160", `"CCC": {"shares": 200, "premium": "260", "breakeven": "38.70"}`,
		`{"defects": [], "alerts": [{"account": "roth", "check": "uncovered_shares", "subject": "BBB 100 sh"}]}`)

	for _, a := range []string{"roth", "trad"} {
		plan := run("history", "plan", "-account", a)
		_ = plan
	}
	rothPulls := []string{"-pull", id + "/roth/orders_pull/window_p1.json", "-pull", id + "/roth/orders_pull/open_p1.json", "-pull", id + "/roth/orders_pull/" + lookupPage(r3Bad) + ".json"}
	run(append([]string{"history", "add", "-account", "roth", "-observed", "2026-02-13T22:00:00Z", "-created-since", "2026-02-06", "-snapshot", id}, rothPulls...)...)
	run("history", "add", "-account", "trad", "-observed", "2026-02-13T22:00:00Z", "-created-since", "2026-02-06", "-pull", id+"/trad/orders_pull/window_p1.json", "-pull", id+"/trad/orders_pull/open_p1.json", "-snapshot", id)
	fmt.Print(run("snapshot", "-snapshot", id))
	sealedAt(id, "2026-02-13T22:05:00Z")
	golden(id, "2026-02-13")

	// reconcile: Robinhood's own realized option P&L for each account, 2026-01-05..2026-02-13
	for _, c := range []struct{ acct, num, total string }{{"roth", "700000001", "250"}, {"trad", "700000002", "100"}} {
		write(layout.PnL("2026-02-13", c.acct), []byte(fmt.Sprintf(`{"_provenance": "SYNTHETIC. Shaped like get_realized_pnl(asset_classes=[option], start_date=2026-01-05, end_date=2026-02-13).",
 "data":{"account_number":%q,"window":"2026-01-05..2026-02-13","display_currency":"USD","total_returns":%q,"total_rate_of_return":"0"}}
`, c.num, c.total)))
	}
}

const readme = `# Synthetic repo root

Everything here is fictional, written by ` + "`go run ./tools/synthfixture`" + ` in the shape of Robinhood's
MCP responses. No real account, holding, price or date of a trade is in it. The order log and
manifests are made by the repo's own commands (history add, snapshot, expirations), so they
are sealed the way real ones are.

Two accounts, **roth** (••0001) and **trad** (••0002); campaign start 2026-01-05. Symbols AAA, BBB, CCC.

## Hand-computed figures (what the tests pin)

The rules are the synthetic ones in books/ira-wheel/rules.json (hand-written example values,
nothing from a real book). Rule 10: the closer is 50% of the fill, rounded up to the tick (a penny under $3).

| Account | Event | Figure |
|---|---|---|
| roth | AAA ×3 $55c exp 1/23 sold $1.00 (credit $300), closer $0.50 filled 1/16 (debit $150) | net **+$150**, realized |
| roth | AAA ×3 $55c exp 3/20 sold $1.50 (credit $450), closer $0.75 | open |
| roth | BBB ×1 $85c exp 2/20 sold $2.00 (credit $200) | open at 2/06 |
| trad | CCC ×2 $44c exp 1/30 sold $0.50 (credit $100), expired worthless (CCC closed 41.20) | net **+$100**, realized |
| trad | CCC ×2 $44c exp 2/20 sold $0.80 (credit $160), closer $0.40 | open |

**Snapshot 2026-02-06** (after close). Roth realized **150**, open credit **650** (450 + 200);
trad realized **100**, open credit **160**. Blocks: AAA 300 sh premium 600 (150 + 450), breakeven
50 − 600/300 = **48.00**; BBB 100 sh premium 200, breakeven 80 − 2 = **78.00**; CCC 200 sh premium 260
(100 + 160), breakeven 40 − 260/200 = **38.70**. One defect: the BBB closer is $1.10, Rule 10 says $1.00.

**Snapshot 2026-02-13.** The $1.10 closer was cancelled and replaced by $1.00, which filled 2/10
(debit $100). Roth realized **250**, open credit **450**; BBB premium 100, breakeven 80 − 1.00 = **79.00**.
Trad unchanged. No defects; one alert, ` + "`uncovered_shares`" + ` on BBB (its call was bought back and not re-sold).
Reconcile 2026-02-13 agrees to $0.00 (roth 250, trad 100).
`
