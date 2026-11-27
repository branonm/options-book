package store

// The write path (the storage design phase 4): open_snapshot, ingest,
// history_add, record_expiration and seal_snapshot, over the create-only DB.
// Each makes the same checks as its file-based counterpart, before writing
// anything, and every write is a create:
//
//   - nothing may be added to a sealed snapshot's sealed roles, and
//     history_add refuses a sealed snapshot before it appends a line (B2);
//   - snapshot ids go through snapshot.ParseID and nothing reads the clock
//     for an as-of date (B1, B3); the clock only stamps when a write happened;
//   - history_add makes history.Log.Prepare's refusals (a window gap, an
//     observation not after the log's last, an open order missing).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/branonm/options-book/internal/layout"
	"io/fs"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/branonm/options-book/internal/history"
	"github.com/branonm/options-book/internal/ledger"
	"github.com/branonm/options-book/internal/snapshot"
)

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// OpenSnapshot starts a snapshot. The id is the one thing that dates it.
func OpenSnapshot(ctx context.Context, db DB, id, note string, now time.Time) error {
	if _, err := snapshot.ParseID(id); err != nil {
		return err
	}
	if note == "" {
		note = "opened through ob-mcp"
	}
	err := db.Create(ctx, CSnapshot, id, Doc{"taken_at": stamp(now), "note": note})
	if errors.Is(err, ErrExists) {
		return fmt.Errorf("snapshot %s already exists; a snapshot is opened once", id)
	}
	return err
}

func requireSnapshot(ctx context.Context, db DB, id string) error {
	if _, err := snapshot.ParseID(id); err != nil {
		return err
	}
	if _, err := db.Get(ctx, CSnapshot, id); errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("no snapshot %s; open_snapshot it first", id)
	} else if err != nil {
		return err
	}
	return nil
}

// IsSealed reports whether a snapshot has its seal.
func IsSealed(ctx context.Context, db DB, id string) (bool, error) {
	_, err := db.Get(ctx, CSeal, id)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	}
	return false, err
}

// Ingest is one raw response to store and attach to a snapshot.
type Ingest struct {
	Snapshot   string
	Role       string
	Account    string // roles held per account
	Instrument string // option_quote/option_instrument: the instrument id; orders_pull: the page name; bars: the file name
	Call       string // the Robinhood MCP call that produced it (for Robinhood roles)
	Args       string // that call's arguments, a JSON object
	Body       []byte
	Verbatim   bool   // false when the response was retyped rather than copied
	Note       string // required when not verbatim: how it was produced
	FetchedAt  string // when the call was made, RFC3339; optional
	Source     string // optional refinement of the source, e.g. "robinhood/mcp-server" for a server-side pull
	Broker     string // the adapter the response is from ("" is Robinhood); decides how it is validated
}

// IngestResult says where a response landed.
type IngestResult struct {
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
	Bytes   int    `json:"bytes"`
	NewBody bool   `json:"new_body"` // false when identical bytes were already stored
}

// IngestResponse validates one response against its role's schema and stores
// it: the body by sha256 (once), then the member that attaches it.
func IngestResponse(ctx context.Context, db DB, in Ingest, now time.Time) (*IngestResult, error) {
	if err := requireSnapshot(ctx, db, in.Snapshot); err != nil {
		return nil, err
	}
	m := Member{Snapshot: in.Snapshot, Role: in.Role, Account: in.Account, Instrument: in.Instrument}
	p, err := m.Path()
	if err != nil {
		return nil, err
	}
	if strings.ContainsAny(in.Instrument, "|/") || strings.ContainsAny(in.Account, "|/") {
		return nil, fmt.Errorf("account and instrument may not contain | or /")
	}
	if sealed(in.Role) {
		ok, err := IsSealed(ctx, db, in.Snapshot)
		if err != nil {
			return nil, err
		}
		if ok {
			return nil, fmt.Errorf("snapshot %s is sealed; nothing can be added to it (only expected, provenance, realized_pnl and bars may follow a seal). Start a new snapshot", in.Snapshot)
		}
	}
	source, call := callFor(in.Role)
	if in.Broker != "" && in.Broker != "robinhood" {
		source = in.Broker // the response is not Robinhood's; its Call is free text
	}
	if in.Source != "" {
		if !strings.HasPrefix(in.Source, source+"/") {
			return nil, fmt.Errorf("source %q doesn't refine %q", in.Source, source)
		}
		source = in.Source
	}
	if source == "robinhood" && in.Call != call {
		return nil, fmt.Errorf("role %s comes from %s, not %q", in.Role, call, in.Call)
	}
	if !in.Verbatim && strings.TrimSpace(in.Note) == "" {
		return nil, fmt.Errorf("a response that isn't verbatim needs a note saying how it was produced (e.g. transcribed from the get_portfolio result, checked field by field)")
	}
	args := "null"
	if strings.TrimSpace(in.Args) != "" {
		var a map[string]any
		if err := json.Unmarshal([]byte(in.Args), &a); err != nil {
			return nil, fmt.Errorf("args must be a JSON object: %w", err)
		}
		b, _ := json.Marshal(a) // keys sorted
		args = string(b)
	}
	if in.FetchedAt != "" {
		if _, err := time.Parse(time.RFC3339, in.FetchedAt); err != nil {
			return nil, fmt.Errorf("fetched_at: %w", err)
		}
	}
	if err := validateBody(in.Role, p, in.Body, in.Broker); err != nil {
		return nil, err
	}

	sha := sum(in.Body)
	res := &IngestResult{Path: p, SHA256: sha, Bytes: len(in.Body)}
	if _, err := db.Get(ctx, CRawResponse, sha); errors.Is(err, fs.ErrNotExist) {
		prov := in.Note
		if prov == "" {
			prov = "verbatim"
		}
		meta := Doc{"source": source, "call": call, "args": args, "account": in.Account, "fetched_at": in.FetchedAt,
			"verbatim": in.Verbatim, "provenance": prov}
		if err := putChunks(ctx, db, sha, in.Body, meta); err != nil {
			return nil, err
		}
		res.NewBody = true
	} else if err != nil {
		return nil, err
	}
	d := Doc{"snapshot_id": m.Snapshot, "role": m.Role, "account": m.Account, "instrument_id": m.Instrument, "sha256": sha}
	if err := db.Create(ctx, CMember, m.ID(), d); errors.Is(err, ErrExists) {
		have, gerr := db.Get(ctx, CMember, m.ID())
		if gerr != nil {
			return nil, gerr
		}
		if have["sha256"] != sha {
			return nil, fmt.Errorf("%s is already stored with different contents; a stored response never changes", p)
		}
	} else if err != nil {
		return nil, err
	}
	return res, nil
}

// putChunks writes a body's chunks, then its raw_response document.
func putChunks(ctx context.Context, db DB, sha string, body []byte, meta Doc) error {
	n := 0
	for off := 0; off < len(body) || n == 0; off += ChunkBytes {
		end := min(off+ChunkBytes, len(body))
		if err := createOrSame(ctx, db, CRawResponse+"/"+sha+"/"+CChunk, fmt.Sprintf("%04d", n), Doc{"body": body[off:end]}); err != nil {
			return err
		}
		n++
	}
	meta["bytes"], meta["chunks"] = int64(len(body)), int64(n)
	if err := db.Create(ctx, CRawResponse, sha, meta); err != nil && !errors.Is(err, ErrExists) {
		return err
	}
	return nil
}

// validateBody checks a response against its role's schema, the way the
// loaders will read it, so a malformed body is refused before it is stored
// (stored documents can't be fixed afterwards).
func validateBody(role, p string, b []byte, brokerName string) error {
	if len(b) == 0 {
		return fmt.Errorf("%s: empty body", p)
	}
	switch role {
	case RoleProvenance:
		if !utf8.Valid(b) {
			return fmt.Errorf("%s: not UTF-8 text", p)
		}
		return nil
	case RoleExpected:
		if !json.Valid(b) {
			return fmt.Errorf("%s: not JSON", p)
		}
		return nil
	case RoleOrdersPull:
		// A page of a multi-page query carries a next cursor; history_add checks the set.
		records, _, err := history.PageRecordsFor(p, b, brokerName)
		if err != nil {
			return err
		}
		for _, raw := range records {
			if _, err := history.OrderHeadFor(raw, brokerName); err != nil {
				return fmt.Errorf("%s: %w", p, err)
			}
		}
		return nil
	}
	// Every other role is a broker response: it must decode, with the decoder
	// the loaders will read it with.
	dec, err := snapshot.At{Broker: brokerName}.Decoder()
	if err != nil {
		return err
	}
	switch role {
	case RoleEquityPositions:
		_, err = dec.EquityPositions(p, b)
	case RoleOptionPositions:
		_, err = dec.OptionPositions(p, b)
	case RolePortfolio:
		_, err = dec.Balance(p, b)
	case RoleEquityQuotes:
		_, err = dec.EquityQuotes(p, b)
	case RoleOptionQuotes, RoleOptionQuote:
		_, err = dec.OptionQuotes(p, b)
	case RoleOptionInstrument:
		_, err = dec.Instruments(p, b)
	case RoleRealizedPnL:
		_, err = dec.RealizedPnL(p, b)
	case RoleBars:
		_, err = dec.Bars(p, b)
	case RoleOptionOrdersAll, RoleOptionOrdersFilled:
		_, err = dec.Orders(p, b)
	default:
		return fmt.Errorf("unknown role %q", role)
	}
	return err
}

// HistoryAdd is one `history add`, with its pull pages already ingested as the
// snapshot's orders_pull members.
type HistoryAdd struct {
	Snapshot     string
	Account      string
	ObservedAt   time.Time
	CreatedSince time.Time // zero with Full
	Full         bool
	Pulls        [][]string // page names per query, in page order; a lookup is its own pull
	Broker       string     // the adapter the account's orders come from ("" is Robinhood)
}

// AddHistory appends the pull to the account's order log and records the
// snapshot's position in it. It refuses before appending anything if the
// snapshot is sealed (B2) or already has a position for the account.
func AddHistory(ctx context.Context, db DB, src *Source, in HistoryAdd) (*history.AddResult, error) {
	if err := requireSnapshot(ctx, db, in.Snapshot); err != nil {
		return nil, err
	}
	if ok, err := IsSealed(ctx, db, in.Snapshot); err != nil {
		return nil, err
	} else if ok {
		return nil, fmt.Errorf("snapshot %s is sealed, so history_add can't record a position in it; nothing was appended to the log. Run history_add before seal_snapshot, or start a new snapshot", in.Snapshot)
	}
	posID := in.Snapshot + "|" + in.Account
	if _, err := db.Get(ctx, COrderPos, posID); err == nil {
		return nil, fmt.Errorf("snapshot %s already has a %s order position; nothing was appended. A snapshot records one history_add per account", in.Snapshot, in.Account)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if len(in.Pulls) == 0 {
		return nil, fmt.Errorf("history_add needs at least one pull")
	}
	if in.Full == !in.CreatedSince.IsZero() {
		return nil, fmt.Errorf("give exactly one of created_since or full")
	}

	add := history.AddInput{ObservedAt: in.ObservedAt, CreatedSince: in.CreatedSince, FullHistory: in.Full}
	pageSHA := map[string]string{} // compact order JSON → page sha256 ("" when in several pages)
	for _, pages := range in.Pulls {
		var names []string
		var bodies [][]byte
		for _, pg := range pages {
			m := Member{Snapshot: in.Snapshot, Role: RoleOrdersPull, Account: in.Account, Instrument: pg}
			p, err := m.Path()
			if err != nil {
				return nil, err
			}
			b, err := fs.ReadFile(src.FS, p)
			if errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("%s: not ingested; ingest the page (role orders_pull, account %s, page %s) first", p, in.Account, pg)
			} else if err != nil {
				return nil, err
			}
			names, bodies = append(names, p), append(bodies, b)
			records, _, err := history.PageRecordsFor(p, b, in.Broker)
			if err != nil {
				return nil, err
			}
			for _, raw := range records {
				var c bytes.Buffer
				if json.Compact(&c, raw) == nil {
					k := c.String()
					if prev, seen := pageSHA[k]; seen && prev != sum(b) {
						pageSHA[k] = ""
					} else {
						pageSHA[k] = sum(b)
					}
				}
			}
		}
		pl, err := history.ParsePullFor(names, bodies, in.Broker)
		if err != nil {
			return nil, err
		}
		add.Pulls = append(add.Pulls, pl)
	}

	logPath := LogPath(in.Account)
	l, err := history.LoadFSFor(src.FS, logPath, in.Broker)
	if err != nil {
		return nil, err
	}
	lines, res, err := l.Prepare(add)
	if err != nil {
		return res, err
	}
	all := append(append([][]byte{}, l.Lines()...), lines...)
	prev := ""
	for _, line := range l.Lines() {
		prev = chainHash(prev, line)
	}
	for i, line := range lines {
		seq := int64(len(l.Lines()) + i + 1)
		var e history.Entry
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, err
		}
		h, err := history.OrderHeadFor(e.Order, in.Broker)
		if err != nil {
			return nil, err
		}
		row := chainHash(prev, line)
		d := Doc{
			"account": in.Account, "seq": seq, "order_id": h.ID, "observed_at": e.ObservedAt,
			"version_key": h.VersionKey, "state": h.State, "created_at": h.CreatedAt,
			"order_json": string(e.Order), "pull_sha256": pageSHA[string(e.Order)],
			"prev_hash": prev, "row_hash": row,
		}
		if in.Broker != "" && in.Broker != "robinhood" {
			d["broker"] = in.Broker
		}
		if err := db.Create(ctx, COrderVersion, orderVersionID(in.Account, seq), d); err != nil {
			return nil, fmt.Errorf("appending line %d to the %s log (another writer may have appended first; re-run history_add): %w", seq, in.Account, err)
		}
		prev = row
	}
	pos := Doc{
		"seq": int64(len(all)), "chain_hash": prev, "log": logPath,
		"prefix_sha256": prefixHash(all), "observed_at": stamp(in.ObservedAt),
	}
	if err := db.Create(ctx, COrderPos, posID, pos); err != nil {
		return nil, err
	}
	return res, nil
}

// RecordExpirations appends outcome records after the ones already stored.
// barSHA is the stored bars response they were read from.
func RecordExpirations(ctx context.Context, db DB, recs []ledger.Expiration, barSHA string, now time.Time) error {
	have, err := db.List(ctx, CExpiration)
	if err != nil {
		return err
	}
	for i, e := range recs {
		d := Doc{
			"contract": e.Contract, "symbol": e.Symbol, "type": e.Type, "strike": e.Strike.String(),
			"expiry": e.Expiry, "underlying_close": e.Close.String(), "outcome": e.Outcome,
			"source": e.Source, "bar_sha256": barSHA, "recorded_at": stamp(now), "ordinal": int64(len(have) + i + 1),
		}
		if err := db.Create(ctx, CExpiration, expirationID(e), d); errors.Is(err, ErrExists) {
			return fmt.Errorf("%s already has a recorded outcome", e.Contract)
		} else if err != nil {
			return err
		}
	}
	return nil
}

// SealResult is a seal that was written.
type SealResult struct {
	Members int // rows the seal covers: sealed members plus order positions
}

// Seal checks a snapshot exactly as `ob snapshot` does and, when nothing is
// missing or inconsistent, writes its seal. Problems come back unsealed.
func Seal(ctx context.Context, db DB, src *Source, id string, refs []snapshot.AccountRef, brokerName string, now time.Time) ([]snapshot.Problem, *SealResult, error) {
	if err := requireSnapshot(ctx, db, id); err != nil {
		return nil, nil, err
	}
	if ok, err := IsSealed(ctx, db, id); err != nil {
		return nil, nil, err
	} else if ok {
		return nil, nil, fmt.Errorf("snapshot %s is already sealed", id)
	}
	if ps := snapshot.CheckAt(withBroker(src.At(id), brokerName), refs); len(ps) > 0 {
		return ps, nil, nil
	}
	var rows []string
	members, err := db.List(ctx, CMember)
	if err != nil {
		return nil, nil, err
	}
	for _, it := range members {
		m, err := parseMemberID(it.ID)
		if err != nil {
			return nil, nil, err
		}
		if m.Snapshot == id && sealed(m.Role) {
			rows = append(rows, it.ID+"|"+it.Doc["sha256"].(string))
		}
	}
	pos, err := db.List(ctx, COrderPos)
	if err != nil {
		return nil, nil, err
	}
	for _, it := range pos {
		if strings.HasPrefix(it.ID, id+"|") {
			rows = append(rows, fmt.Sprintf("%s|%d|%s", it.ID, it.Doc["seq"].(int64), it.Doc["chain_hash"].(string)))
		}
	}
	sort.Strings(rows)
	d := Doc{"sealed_at": stamp(now), "members": int64(len(rows)), "members_hash": sealHash(rows)}
	if err := db.Create(ctx, CSeal, id, d); errors.Is(err, ErrExists) {
		return nil, nil, fmt.Errorf("snapshot %s is already sealed", id)
	} else if err != nil {
		return nil, nil, err
	}
	return nil, &SealResult{Members: len(rows)}, nil
}

// MemberFor maps a file in snapshots/<snap>/ to the member ingest would make
// of it; ok is false for files that aren't members (history.json,
// manifest.json, generated output).
func MemberFor(snap, rel string) (Member, bool, error) {
	kind, m, err := classify(snap, rel)
	if err != nil {
		return Member{}, false, err
	}
	return m, kind == kindMember, nil
}

// BarsPath is where an ingested bars response sits in the repo layout.
func BarsPath(name string) string { return layout.Bars(name) }

// Raw is a response stored on its own, attached to no snapshot: an rh_read
// lookup, or a contract a ticket fetched for itself (PLAN.md, addendum A).
type Raw struct {
	Call      string // the Robinhood MCP tool
	Args      string // its arguments, a JSON object
	Body      []byte
	FetchedAt time.Time
	Source    string // e.g. robinhood/mcp-server
}

// StoreRaw stores a verbatim response by sha256 and returns the sha256.
// Bodies are content-addressed: identical bytes already stored keep their
// first metadata, and newBody is false.
func StoreRaw(ctx context.Context, db DB, r Raw) (sha string, newBody bool, err error) {
	if r.Call == "" || len(r.Body) == 0 || r.FetchedAt.IsZero() {
		return "", false, fmt.Errorf("a stored response needs its call, body and fetched_at")
	}
	// Robinhood pulls, and the one Alpaca paper source (read-only GETs).
	if !strings.HasPrefix(r.Source, "robinhood/") && r.Source != "alpaca/paper-rest" {
		return "", false, fmt.Errorf("source %q: only Robinhood and Alpaca paper pulls are stored this way", r.Source)
	}
	if !utf8.Valid(r.Body) {
		return "", false, fmt.Errorf("%s: the body isn't UTF-8 text", r.Call)
	}
	args := "null"
	if strings.TrimSpace(r.Args) != "" {
		var a map[string]any
		if err := json.Unmarshal([]byte(r.Args), &a); err != nil {
			return "", false, fmt.Errorf("args must be a JSON object: %w", err)
		}
		b, _ := json.Marshal(a)
		args = string(b)
	}
	sha = sum(r.Body)
	if _, err := db.Get(ctx, CRawResponse, sha); err == nil {
		return sha, false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", false, err
	}
	meta := Doc{"source": r.Source, "call": r.Call, "args": args, "account": "",
		"fetched_at": r.FetchedAt.UTC().Format(time.RFC3339Nano), "verbatim": true, "provenance": "verbatim"}
	if err := putChunks(ctx, db, sha, r.Body, meta); err != nil {
		return "", false, err
	}
	return sha, true, nil
}

// RawBody reads a stored response back by sha256, checking it.
func RawBody(ctx context.Context, db DB, sha string) ([]byte, error) {
	d, err := db.Get(ctx, CRawResponse, sha)
	if err != nil {
		return nil, err
	}
	return readBody(ctx, db, sha, d)
}

// LatestSealed is the most recently sealed snapshot and when it was sealed.
func LatestSealed(ctx context.Context, db DB) (id, sealedAt string, err error) {
	seals, err := db.List(ctx, CSeal)
	if err != nil {
		return "", "", err
	}
	for _, it := range seals {
		at, _ := it.Doc["sealed_at"].(string)
		if at > sealedAt {
			id, sealedAt = it.ID, at
		}
	}
	if id == "" {
		return "", "", fmt.Errorf("no snapshot is sealed yet")
	}
	return id, sealedAt, nil
}

func withBroker(at snapshot.At, name string) snapshot.At {
	at.Broker = name
	return at
}
