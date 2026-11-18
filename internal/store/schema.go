package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/branonm/options-book/internal/layout"
	"path"
	"sort"
	"strings"
)

// Collections, as named in the storage design.
const (
	CRawResponse  = "raw_response"
	CChunk        = "chunk" // subcollection of raw_response/<sha256>
	CSnapshot     = "snapshot"
	CMember       = "snapshot_member"
	CSeal         = "snapshot_seal"
	COrderPos     = "snapshot_order_pos"
	COrderVersion = "order_version"
	CExpiration   = "expiration_outcome"
	CRun          = "run"
)

// Collections lists every top-level collection, in export order.
var Collections = []string{CRawResponse, CSnapshot, CMember, CSeal, COrderPos, COrderVersion, CExpiration, CRun}

// ChunkBytes is the most body bytes one chunk document holds (spec: 512 KiB).
const ChunkBytes = 512 << 10

// Member roles. The first block is the storage design's list; the second is
// what today's snapshots also hold, which the spec's list does not name
// (recorded as an addition for the spec: see PLAN.md).
const (
	RoleEquityPositions  = "equity_positions"
	RoleOptionPositions  = "option_positions"
	RolePortfolio        = "portfolio"
	RoleEquityQuotes     = "equity_quotes"
	RoleOptionQuotes     = "option_quotes"
	RoleOptionQuote      = "option_quote"
	RoleOptionInstrument = "option_instrument"
	RoleRealizedPnL      = "realized_pnl"
	RoleExpected         = "expected"

	RoleOrdersPull         = "orders_pull"          // <acct>/orders_pull/<page>.json; instrument holds the page name
	RoleOptionOrdersAll    = "option_orders_all"    // pre-log snapshots: a full saved history
	RoleOptionOrdersFilled = "option_orders_filled" // pre-log snapshots: fills only
	RoleProvenance         = "provenance"           // PROVENANCE.md
)

// sealed reports whether a role is covered by the snapshot's seal. These are
// exactly the files `ob snapshot` puts in manifest.json (every .json raw
// input except expected.json). expected and provenance are notes about the
// snapshot, not inputs to it, and a realized-P&L pull is a reconciliation
// input that may come after the seal (11/07's did), so those three may be
// attached to a sealed snapshot, as may day bars (history/bars/, ingested to
// record an expiration outcome after the check-in).
func sealed(role string) bool {
	switch role {
	case RoleExpected, RoleProvenance, RoleRealizedPnL, RoleBars:
		return false
	}
	return true
}

// Member is one snapshot_member document's key.
type Member struct {
	Snapshot, Role, Account, Instrument string
}

// ID is the document id: <snapshot>|<role>|<account>|<instrument>.
func (m Member) ID() string {
	return strings.Join([]string{m.Snapshot, m.Role, m.Account, m.Instrument}, "|")
}

func parseMemberID(id string) (Member, error) {
	p := strings.Split(id, "|")
	if len(p) != 4 {
		return Member{}, fmt.Errorf("member id %q: want snapshot|role|account|instrument", id)
	}
	return Member{p[0], p[1], p[2], p[3]}, nil
}

// Path is where the member lives in the repo layout, relative to the repo root.
// roleFile is the file name each snapshot-file role is stored under (the
// layout's name for it), in both directions: Member.Path builds with it and
// classify reads with it.
var roleFile = map[string]string{
	RoleEquityPositions:    layout.EquityPositions,
	RoleOptionPositions:    layout.OptionPositions,
	RolePortfolio:          layout.Portfolio,
	RoleOptionOrdersAll:    layout.OptionOrdersAll,
	RoleOptionOrdersFilled: layout.OptionOrdersFilled,
	RoleEquityQuotes:       layout.EquityQuotes,
	RoleOptionQuotes:       layout.OptionQuotes,
	RoleExpected:           layout.Expected,
}

// roleOfFile is roleFile inverted.
var roleOfFile = func() map[string]string {
	m := map[string]string{}
	for role, file := range roleFile {
		m[file] = role
	}
	return m
}()

func (m Member) Path() (string, error) {
	snap := layout.Snapshot(m.Snapshot)
	need := func(acct, inst bool) error {
		if (m.Account != "") != acct || (m.Instrument != "") != inst {
			return fmt.Errorf("member %s: role %s takes account=%v instrument=%v", m.ID(), m.Role, acct, inst)
		}
		return nil
	}
	switch m.Role {
	case RoleEquityPositions, RoleOptionPositions, RolePortfolio, RoleOptionOrdersAll, RoleOptionOrdersFilled:
		return path.Join(snap, layout.AccountFile(m.Account, roleFile[m.Role])), need(true, false)
	case RoleEquityQuotes, RoleOptionQuotes, RoleExpected:
		return path.Join(snap, roleFile[m.Role]), need(false, false)
	case RoleProvenance:
		return path.Join(snap, layout.Provenance), need(false, false)
	case RoleOptionQuote:
		return path.Join(snap, layout.OptionFile(m.Instrument, layout.QuoteFile)), need(false, true)
	case RoleOptionInstrument:
		return path.Join(snap, layout.OptionFile(m.Instrument, layout.InstrumentFile)), need(false, true)
	case RoleOrdersPull:
		return path.Join(snap, layout.OrdersPull(m.Account, m.Instrument)), need(true, true)
	case RoleRealizedPnL:
		return layout.PnL(m.Snapshot, m.Account), need(true, false)
	case RoleBars:
		return BarsPath(m.Instrument), need(false, true)
	}
	return "", fmt.Errorf("member %s: unknown role %q", m.ID(), m.Role)
}

// What a file inside snapshots/<id>/ is, for the importer.
type fileKind int

const (
	kindMember    fileKind = iota
	kindHistory            // <acct>/history.json → snapshot_order_pos
	kindManifest           // manifest.json → snapshot_seal
	kindGenerated          // wheel-book.generated.md: derived output (a test golden), not stored
)

// classify maps a path relative to a snapshot folder to what it becomes.
// Anything it does not recognise is an error, not a guess.
func classify(snap, rel string) (fileKind, Member, error) {
	m := Member{Snapshot: snap}
	p := strings.Split(rel, "/")
	switch {
	case rel == layout.Manifest:
		return kindManifest, m, nil
	case rel == layout.GeneratedBook:
		return kindGenerated, m, nil
	case rel == layout.Provenance:
		m.Role = RoleProvenance
	case len(p) == 1 && (rel == layout.EquityQuotes || rel == layout.OptionQuotes || rel == layout.Expected):
		m.Role = roleOfFile[rel]
	case len(p) == 3 && p[0] == layout.OptionsDir && (p[2] == layout.QuoteFile || p[2] == layout.InstrumentFile):
		m.Instrument = p[1]
		m.Role = map[string]string{layout.QuoteFile: RoleOptionQuote, layout.InstrumentFile: RoleOptionInstrument}[p[2]]
	case len(p) == 2 && p[1] == layout.HistoryRef:
		m.Account = p[0]
		return kindHistory, m, nil
	case len(p) == 2:
		m.Account = p[0]
		switch p[1] {
		case layout.EquityPositions, layout.OptionPositions, layout.Portfolio, layout.OptionOrdersAll, layout.OptionOrdersFilled:
			m.Role = roleOfFile[p[1]]
		}
	case len(p) == 3 && p[1] == layout.OrdersPullDir && strings.HasSuffix(p[2], ".json"):
		m.Account, m.Role, m.Instrument = p[0], RoleOrdersPull, strings.TrimSuffix(p[2], ".json")
	}
	if m.Role == "" {
		return 0, m, fmt.Errorf("%s: not a file the store knows how to hold", layout.InSnapshot(snap, rel))
	}
	return kindMember, m, nil
}

// callFor is the Robinhood MCP call (or options-book source) that produces a role.
func callFor(role string) (source, call string) {
	switch role {
	case RoleEquityPositions:
		return "robinhood", "get_equity_positions"
	case RoleOptionPositions:
		return "robinhood", "get_option_positions"
	case RolePortfolio:
		return "robinhood", "get_portfolio"
	case RoleEquityQuotes:
		return "robinhood", "get_equity_quotes"
	case RoleOptionQuotes, RoleOptionQuote:
		return "robinhood", "get_option_quotes"
	case RoleOptionInstrument:
		return "robinhood", "get_option_instruments"
	case RoleRealizedPnL:
		return "robinhood", "get_realized_pnl"
	case RoleOrdersPull, RoleOptionOrdersAll, RoleOptionOrdersFilled:
		return "robinhood", "get_option_orders"
	case RoleBars:
		return "robinhood", "get_equity_historicals"
	}
	return "options-book", role
}

// RoleBars marks a raw_response holding day bars (history/bars/*.json). Bars
// are not snapshot members; expiration_outcome.bar_sha256 points at them.
const RoleBars = "bars"

// Order-version ids: <account>|<seq, 8 digits>.
func orderVersionID(acct string, seq int64) string { return fmt.Sprintf("%s|%08d", acct, seq) }

// chainHash links one order-log line to the one before it:
// row_hash = sha256(prev_hash || "\n" || line), with prev_hash "" for seq 1.
// verify re-walks it; any edit, reorder or deletion breaks every later hash.
func chainHash(prev string, line []byte) string {
	h := sha256.New()
	h.Write([]byte(prev))
	h.Write([]byte{'\n'})
	h.Write(line)
	return hex.EncodeToString(h.Sum(nil))
}

// sealRows are the lines a seal's members_hash covers: one per sealed member
// ("<member id>|<sha256>") and one per order position
// ("<pos id>|<seq>|<chain_hash>"), sorted. Adding, removing or changing any of
// them changes the hash, and every read of the snapshot then fails.
func sealHash(rows []string) string {
	sort.Strings(rows)
	h := sha256.New()
	for _, r := range rows {
		h.Write([]byte(r))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// CallFor is the source and call that produce a role's responses.
func CallFor(role string) (source, call string) { return callFor(role) }
