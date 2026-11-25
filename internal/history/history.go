// Package history keeps each account's option-order history as an
// append-only log of observed order versions:
//
//	history/<account>/orders.jsonl   one JSON object per line: {"observed_at", "order"}
//
// A line is appended when an order is first seen or when its state, fills,
// quantity, price or time in force change (not on a broker's housekeeping
// touch of a resting order, such as Robinhood's daily updated_at bump). The state of the book at any moment is the
// latest version of each order observed at or before it, so a snapshot can
// be replayed exactly from a prefix of the log no matter what is appended
// later.
//
// A broker's order query may filter by creation date only (Robinhood's does),
// but old orders change state (a closer created in October fills in November). Add
// therefore requires every order that was open in the log to be accounted
// for in the new pull — by a created-since window, the open-order list, or a
// single-order lookup — and refuses the pull otherwise.
package history

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/branonm/options-book/internal/broker"
)

// Entry is one observed version of one order.
type Entry struct {
	ObservedAt string          `json:"observed_at"`
	Order      json.RawMessage `json:"order"`
}

// defaultSync is the order sync used until config/accounts.json names a source
// per account (item 6 step 3).
func defaultSync() (broker.OrderSync, error) { return broker.LookupSync(broker.Default) }

// syncFor is the order sync for a source, "" meaning the default broker.
func syncFor(source string) (broker.OrderSync, error) {
	if source == "" {
		return defaultSync()
	}
	return broker.LookupSync(source)
}

// Head is the part of an order record the log compares: its id, state,
// creation time and the version key that decides whether a new observation
// is a new line (the broker's own housekeeping touches excluded; see
// broker.OrderHead.Version).
type Head struct {
	ID, State, CreatedAt, VersionKey string
}

// OrderHead parses an order record the way the log does.
func OrderHead(raw json.RawMessage) (Head, error) { return OrderHeadFor(raw, "") }

// OrderHeadFor is OrderHead for a record from the named source ("" is the default broker).
func OrderHeadFor(raw json.RawMessage, source string) (Head, error) {
	s, err := syncFor(source)
	if err != nil {
		return Head{}, err
	}
	h, err := s.Head(raw)
	if err != nil {
		return Head{}, err
	}
	return Head{ID: h.ID, State: h.State, CreatedAt: h.CreatedAt, VersionKey: h.Version}, nil
}

// Log is a loaded history file.
type Log struct {
	Path    string
	Entries []Entry
	lines   [][]byte // raw lines, for prefix hashing
	sync    broker.OrderSync
	// observed is the latest observed_at of any snapshot position recorded for
	// this account (a history add that appended nothing leaves no line, only
	// that position). See LastObserved.
	observed string
}

// head reads an order record's head with the log's broker.
func (l *Log) head(raw []byte) (broker.OrderHead, error) { return l.sync.Head(raw) }

// Load reads a log; a missing file is an empty log.
func Load(path string) (*Log, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return newLog(path, "")
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	l, err := parse(path, f, "")
	if err != nil {
		return nil, err
	}
	// <root>/history/<account>/orders.jsonl: the snapshots sit under <root>.
	dir := filepath.Dir(path)
	if filepath.Base(filepath.Dir(dir)) == "history" {
		l.observed = latestPosition(os.DirFS(filepath.Dir(filepath.Dir(dir))), filepath.Base(dir))
	}
	return l, nil
}

// LoadFS is Load over a read-only Source view (fs.FS); name is a slash path
// such as history/roth/orders.jsonl. A missing log is an empty log.
func LoadFS(fsys fs.FS, name string) (*Log, error) { return LoadFSFor(fsys, name, "") }

// LoadFSFor is LoadFS for a log whose orders come from the named source (an
// adapter name, "" for the default broker). The source decides how the log reads
// an order record's head and which states are final.
func LoadFSFor(fsys fs.FS, name, source string) (*Log, error) {
	f, err := fsys.Open(name)
	if errors.Is(err, fs.ErrNotExist) {
		return newLog(name, source)
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	l, err := parse(name, f, source)
	if err != nil {
		return nil, err
	}
	// history/<account>/orders.jsonl: the snapshots sit beside history/.
	if dir := path.Dir(name); path.Base(path.Dir(dir)) == "history" {
		l.observed = latestPosition(fsys, path.Base(dir))
	}
	return l, nil
}

// latestPosition is the latest observed_at among the account's snapshot
// positions (snapshots/*/<account>/history.json), "" when there is none. A
// position that can't be read is skipped: that only leaves the window as wide
// as the log alone would have made it.
func latestPosition(fsys fs.FS, account string) string {
	names, _ := fs.Glob(fsys, "snapshots/*/"+account+"/history.json")
	last := ""
	for _, n := range names {
		b, err := fs.ReadFile(fsys, n)
		if err != nil {
			continue
		}
		var ref Ref
		if json.Unmarshal(b, &ref) == nil && ref.ObservedAt > last {
			last = ref.ObservedAt
		}
	}
	return last
}

// Lines returns the raw bytes of each line (without the newline), exactly as
// stored. Callers must not modify them.
func (l *Log) Lines() [][]byte { return l.lines }

func newLog(path, source string) (*Log, error) {
	s, err := syncFor(source)
	if err != nil {
		return nil, err
	}
	return &Log{Path: path, sync: s}, nil
}

func parse(path string, r io.Reader, source string) (*Log, error) {
	l, err := newLog(path, source)
	if err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for n := 1; sc.Scan(); n++ {
		line := append([]byte{}, sc.Bytes()...)
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, n, err)
		}
		if _, err := l.head(e.Order); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, n, err)
		}
		l.Entries = append(l.Entries, e)
		l.lines = append(l.lines, line)
	}
	return l, sc.Err()
}

// PrefixHash fingerprints the first n lines.
func (l *Log) PrefixHash(n int) (string, error) {
	if n < 0 || n > len(l.lines) {
		return "", fmt.Errorf("%s has %d lines, not %d", l.Path, len(l.lines), n)
	}
	h := sha256.New()
	for _, line := range l.lines[:n] {
		h.Write(line)
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// OrderRecords returns the latest version of each order among the first n
// lines as raw order records, newest first (by created_at), as the broker
// lists them. The log doesn't interpret an order beyond its head; a broker
// adapter decodes each record (broker.Decoder.OrderRecord).
func (l *Log) OrderRecords(n int) ([]json.RawMessage, error) {
	if n > len(l.Entries) {
		return nil, fmt.Errorf("%s has %d lines, not %d", l.Path, len(l.Entries), n)
	}
	latest := map[string]json.RawMessage{}
	for _, e := range l.Entries[:n] {
		h, _ := l.head(e.Order)
		latest[h.ID] = e.Order
	}
	type rec struct {
		created string
		raw     json.RawMessage
	}
	recs := make([]rec, 0, len(latest))
	for _, raw := range latest {
		h, _ := l.head(raw)
		recs = append(recs, rec{h.CreatedAt, raw})
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].created > recs[j].created })
	out := make([]json.RawMessage, len(recs))
	for i, r := range recs {
		out[i] = r.raw
	}
	return out, nil
}

// current is the latest head per id across the whole log.
func (l *Log) current() map[string]broker.OrderHead {
	m := map[string]broker.OrderHead{}
	for _, e := range l.Entries {
		h, _ := l.head(e.Order)
		m[h.ID] = h
	}
	return m
}

// OpenIDs lists orders whose latest logged version is not terminal.
func (l *Log) OpenIDs() []string {
	var ids []string
	for id, h := range l.current() {
		if !l.sync.Terminal(h.State) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// LastObserved is the latest time the log was observed complete ("" when it
// never was): the latest observed_at of a line or of a snapshot's position in
// the log. A pull that found nothing new appends no line but is still an
// observation (B12), so a quiet account's window moves forward with it.
func (l *Log) LastObserved() string {
	last := l.observed
	for _, e := range l.Entries {
		if e.ObservedAt > last {
			last = e.ObservedAt
		}
	}
	return last
}

// Pull is one fetch of orders: a full history, a created-since window, the
// open-order list, or single-order lookups, possibly over several pages.
type Pull struct {
	Orders []json.RawMessage
	Files  []string
}

// ReadPull reads one or more saved order-list responses. Files given
// together are pages of one query, in order: every page but the last must
// carry a next cursor and the last must not, so a query cut off mid-way is
// refused rather than silently undercounted. Single-order lookups are passed
// as separate one-file pulls.
func ReadPull(files ...string) (*Pull, error) {
	if len(files) == 0 {
		return nil, fmt.Errorf("no files")
	}
	bodies := make([][]byte, len(files))
	for i, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		bodies[i] = b
	}
	return ParsePull(files, bodies)
}

// ParsePull is ReadPull over pages already read: names label them in errors
// and in Pull.Files, bodies are the saved responses, in page order.
func ParsePull(names []string, bodies [][]byte) (*Pull, error) {
	return ParsePullFor(names, bodies, "")
}

// ParsePullFor is ParsePull for pages from the named source ("" is the default broker).
func ParsePullFor(names []string, bodies [][]byte, source string) (*Pull, error) {
	if len(names) == 0 || len(names) != len(bodies) {
		return nil, fmt.Errorf("want one name per page")
	}
	s, err := syncFor(source)
	if err != nil {
		return nil, err
	}
	files := names
	p := &Pull{Files: files}
	for i, f := range files {
		page, err := s.Page(f, bodies[i])
		if err != nil {
			return nil, err
		}
		more := page.Next != ""
		last := i == len(files)-1
		switch {
		case last && more:
			return nil, fmt.Errorf("%s: last page still has a next cursor; fetch the remaining pages", f)
		case !last && !more:
			return nil, fmt.Errorf("%s: page %d has no next cursor, but more pages were given", f, i+1)
		}
		for _, raw := range page.Records {
			if _, err := s.Head(raw); err != nil {
				return nil, fmt.Errorf("%s: %w", f, err)
			}
			p.Orders = append(p.Orders, compact(raw))
		}
	}
	return p, nil
}

func compact(raw json.RawMessage) json.RawMessage {
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return raw
	}
	return b.Bytes()
}

// AddInput is one incremental update.
type AddInput struct {
	ObservedAt time.Time
	// CreatedSince is the created_at_gte used for the window pull, or zero
	// when the pull is a full history. It must reach back to (or before) the
	// log's last observation, or orders created in the gap would be missed.
	CreatedSince time.Time
	FullHistory  bool
	Pulls        []*Pull
}

// AddResult reports what an update changed.
type AddResult struct {
	Appended   int      `json:"appended"`
	NewOrders  int      `json:"new_orders"`
	Changed    int      `json:"changed_orders"`
	Unchanged  int      `json:"unchanged"`
	Lines      int      `json:"lines"`
	MissingIDs []string `json:"missing_ids,omitempty"`
}

// Plan says what an incremental pull must contain.
type Plan struct {
	CreatedSince string   `json:"created_at_gte"`
	RefreshIDs   []string `json:"refresh_order_ids"`
}

// NextPlan describes the next incremental pull: a created-since window that
// overlaps the last observation by a day, plus every order open in the log
// (which the open-order list or single lookups must cover).
func (l *Log) NextPlan() Plan {
	p := Plan{RefreshIDs: l.OpenIDs()}
	if last, err := time.Parse(time.RFC3339, l.LastObserved()); err == nil {
		p.CreatedSince = last.AddDate(0, 0, -1).Format("2006-01-02")
	}
	return p
}

// Add appends the pull to the log, after checking it is complete.
func (l *Log) Add(in AddInput) (*AddResult, error) {
	add, res, err := l.Prepare(in)
	if err != nil || len(add) == 0 {
		return res, err
	}
	if err := os.MkdirAll(filepath.Dir(l.Path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(l.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	for _, line := range add {
		if _, err := f.Write(append(line, '\n')); err != nil {
			f.Close()
			return nil, err
		}
		l.appendLine(line)
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	res.Appended = len(add)
	res.Lines = len(l.lines)
	return res, nil
}

// appendLine records a line already written to the log's store.
func (l *Log) appendLine(line []byte) {
	var e Entry
	json.Unmarshal(line, &e)
	l.Entries = append(l.Entries, e)
	l.lines = append(l.lines, line)
}

// Prepare runs every check Add makes and returns the lines Add would
// append, without writing anything or changing l. res.Lines is the log's
// length after those lines; res.Appended is len(lines). A store that appends
// them itself (internal/store) gets exactly the file log's refusals and lines.
func (l *Log) Prepare(in AddInput) ([][]byte, *AddResult, error) {
	res := &AddResult{}
	seen := map[string]json.RawMessage{}
	for _, p := range in.Pulls {
		for _, raw := range p.Orders {
			h, _ := l.head(raw)
			// Within one update, keep the most recently updated version.
			if prev, ok := seen[h.ID]; ok {
				ph, _ := l.head(prev)
				if ph.UpdatedAt >= h.UpdatedAt {
					continue
				}
			}
			seen[h.ID] = raw
		}
	}

	if len(l.Entries) > 0 && !in.FullHistory {
		last, err := time.Parse(time.RFC3339, l.LastObserved())
		if err != nil {
			return nil, nil, fmt.Errorf("log's last observed_at is unreadable: %w", err)
		}
		if in.CreatedSince.IsZero() || in.CreatedSince.After(last) {
			return nil, nil, fmt.Errorf("created-since window starts %s, after the log's last observation %s; orders created in the gap would be missed",
				in.CreatedSince.Format("2006-01-02"), last.Format(time.RFC3339))
		}
		if !in.ObservedAt.After(last) {
			return nil, nil, fmt.Errorf("observed_at %s is not after the log's last observation %s", in.ObservedAt.Format(time.RFC3339), last.Format(time.RFC3339))
		}
		for _, id := range l.OpenIDs() {
			if _, ok := seen[id]; !ok {
				res.MissingIDs = append(res.MissingIDs, id)
			}
		}
		if len(res.MissingIDs) > 0 {
			return nil, res, fmt.Errorf("%d order(s) open in the log are not in this pull; fetch each with %s: %s",
				len(res.MissingIDs), l.sync.Lookup(), strings.Join(res.MissingIDs, ", "))
		}
	}

	cur := l.current()
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	// Append in creation order so the log reads chronologically.
	sort.Slice(ids, func(i, j int) bool {
		hi, _ := l.head(seen[ids[i]])
		hj, _ := l.head(seen[ids[j]])
		if hi.CreatedAt != hj.CreatedAt {
			return hi.CreatedAt < hj.CreatedAt
		}
		return ids[i] < ids[j]
	})
	obs := in.ObservedAt.UTC().Format(time.RFC3339)
	var add [][]byte
	for _, id := range ids {
		h, _ := l.head(seen[id])
		prev, existed := cur[id]
		switch {
		case !existed:
			res.NewOrders++
		case prev.Version == h.Version:
			res.Unchanged++
			continue
		case prev.UpdatedAt > h.UpdatedAt:
			// An older version than the log already has: ignore.
			res.Unchanged++
			continue
		default:
			res.Changed++
		}
		line, err := json.Marshal(Entry{ObservedAt: obs, Order: seen[id]})
		if err != nil {
			return nil, nil, err
		}
		add = append(add, line)
	}
	res.Appended = len(add)
	res.Lines = len(l.lines) + len(add)
	return add, res, nil
}

// Ref is what a snapshot records to replay the log exactly.
type Ref struct {
	Log        string `json:"log"`         // path relative to the repo root
	Lines      int    `json:"lines"`       // replay the first N lines
	SHA256     string `json:"sha256"`      // fingerprint of those lines
	ObservedAt string `json:"observed_at"` // the pull this snapshot reflects
}

// PlanSteps says, one line each, what the next incremental pull must
// contain, in the broker's own terms (broker.OrderSync.PullSteps).
func (l *Log) PlanSteps(p Plan) []string { return l.sync.PullSteps(p.CreatedSince, p.RefreshIDs) }

// FullPull says how to pull an account's entire history, for an empty log.
func (l *Log) FullPull() string { return l.sync.FullPull() }

// PageRecords reads one saved order-list page: its order records, exactly as
// the broker sent them, and the cursor for the next page ("" on the last).
// The log doesn't refuse a cursor here; ParsePull checks a pull is complete.
func PageRecords(name string, body []byte) (records []json.RawMessage, next string, err error) {
	return PageRecordsFor(name, body, "")
}

// PageRecordsFor is PageRecords for a page from the named source ("" is the default broker).
func PageRecordsFor(name string, body []byte, source string) (records []json.RawMessage, next string, err error) {
	s, err := syncFor(source)
	if err != nil {
		return nil, "", err
	}
	page, err := s.Page(name, body)
	if err != nil {
		return nil, "", err
	}
	return page.Records, page.Next, nil
}
