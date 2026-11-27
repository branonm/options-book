package store

import (
	"bytes"
	"context"
	"fmt"
	"github.com/branonm/options-book/internal/layout"
	"io/fs"
	"sort"
	"strings"
	"testing/fstest"

	"github.com/shopspring/decimal"

	"github.com/branonm/options-book/internal/history"
	"github.com/branonm/options-book/internal/ledger"
	"github.com/branonm/options-book/internal/snapshot"
)

// Source is the store read back as the repo layout: snapshots/<id>/…,
// history/<acct>/orders.jsonl, history/expirations.json and reconcile/<id>/…,
// with history.json, manifest.json, the order logs and expirations.json
// regenerated from their documents. Every loader reads it exactly as it reads
// the files (snapshot.At{FS: src.FS, Dir: "snapshots/<id>"}).
//
// Open verifies everything it reads and refuses the whole store on any
// mismatch: a body whose sha256 isn't its id, a member pointing at a missing
// body, a broken order-log chain, a position whose hash doesn't match, or a
// seal whose count or hash no longer matches its members.
type Source struct {
	FS        fs.FS
	Snapshots []string // ids, sorted
}

// At is the snapshot.At for one snapshot id.
func (s *Source) At(id string) snapshot.At { return snapshot.At{FS: s.FS, Dir: layout.Snapshot(id)} }

// Open reads every collection from db and builds the Source.
func Open(ctx context.Context, db DB) (*Source, error) {
	r := &reader{ctx: ctx, db: db, files: fstest.MapFS{}}
	if err := r.load(); err != nil {
		return nil, err
	}
	return &Source{FS: r.files, Snapshots: r.snaps}, nil
}

type reader struct {
	ctx    context.Context
	db     DB
	files  fstest.MapFS
	snaps  []string
	bodies map[string]Doc
	chains map[string][]string // account → row hash per seq
	lines  map[string][][]byte // account → log lines
}

func (r *reader) put(p string, b []byte) { r.files[p] = &fstest.MapFile{Data: b, Mode: 0o444} }

func (r *reader) load() error {
	var err error
	if r.bodies, err = r.index(CRawResponse); err != nil {
		return err
	}
	if err := r.loadLogs(); err != nil {
		return err
	}
	if err := r.loadExpirations(); err != nil {
		return err
	}
	snaps, err := r.index(CSnapshot)
	if err != nil {
		return err
	}
	members, err := r.db.List(r.ctx, CMember)
	if err != nil {
		return err
	}
	pos, err := r.db.List(r.ctx, COrderPos)
	if err != nil {
		return err
	}
	seals, err := r.index(CSeal)
	if err != nil {
		return err
	}
	rows := map[string][]string{}
	manifests := map[string]map[string]string{}
	for id := range snaps {
		if _, err := snapshot.ParseID(id); err != nil {
			return err
		}
		r.snaps = append(r.snaps, id)
		manifests[id] = map[string]string{}
	}
	sort.Strings(r.snaps)

	for _, it := range members {
		m, err := parseMemberID(it.ID)
		if err != nil {
			return err
		}
		if err := r.checkMember(m, it.Doc); err != nil {
			return err
		}
		if _, ok := snaps[m.Snapshot]; !ok {
			return fmt.Errorf("%s/%s: no snapshot %s", CMember, it.ID, m.Snapshot)
		}
		sha, _ := str(it.Doc, "sha256")
		body, err := r.body(sha)
		if err != nil {
			return fmt.Errorf("%s/%s: %w", CMember, it.ID, err)
		}
		p, err := m.Path()
		if err != nil {
			return err
		}
		if _, dup := r.files[p]; dup {
			return fmt.Errorf("%s/%s: two members claim %s", CMember, it.ID, p)
		}
		r.put(p, body)
		if sealed(m.Role) {
			rows[m.Snapshot] = append(rows[m.Snapshot], it.ID+"|"+sha)
			manifests[m.Snapshot][strings.TrimPrefix(p, layout.Snapshot(m.Snapshot)+"/")] = sha
		}
	}

	for _, it := range pos {
		snap, acct, ok := strings.Cut(it.ID, "|")
		if _, known := snaps[snap]; !ok || !known {
			return fmt.Errorf("%s/%s: not <snapshot>|<account> for a known snapshot", COrderPos, it.ID)
		}
		seq, err := i64(it.Doc, "seq")
		if err != nil {
			return fmt.Errorf("%s/%s: %w", COrderPos, it.ID, err)
		}
		chain := r.chains[acct]
		if seq < 0 || int(seq) > len(chain) {
			return fmt.Errorf("%s/%s: seq %d, but the %s log has %d lines", COrderPos, it.ID, seq, acct, len(chain))
		}
		if h, err := str(it.Doc, "chain_hash"); err != nil || h != chainAt(chain, seq) {
			return fmt.Errorf("%s/%s: chain_hash doesn't match the %s log at seq %d", COrderPos, it.ID, acct, seq)
		}
		if lp, err := str(it.Doc, "log"); err != nil || lp != LogPath(acct) {
			return fmt.Errorf("%s/%s: log must be %s", COrderPos, it.ID, LogPath(acct))
		}
		if ps, err := str(it.Doc, "prefix_sha256"); err != nil || ps != prefixHash(r.lines[acct][:seq]) {
			return fmt.Errorf("%s/%s: prefix_sha256 doesn't match the first %d lines of the %s log", COrderPos, it.ID, seq, acct)
		}
		b, err := historyRef(it.Doc)
		if err != nil {
			return fmt.Errorf("%s/%s: %w", COrderPos, it.ID, err)
		}
		r.put(layout.InSnapshot(snap, layout.AccountFile(acct, layout.HistoryRef)), b)
		rows[snap] = append(rows[snap], fmt.Sprintf("%s|%d|%s", it.ID, seq, chainAt(chain, seq)))
		manifests[snap][layout.AccountFile(acct, layout.HistoryRef)] = sum(b)
	}

	for id, seal := range seals {
		if _, ok := snaps[id]; !ok {
			return fmt.Errorf("%s/%s: no such snapshot", CSeal, id)
		}
		n, err1 := i64(seal, "members")
		h, err2 := str(seal, "members_hash")
		at, err3 := str(seal, "sealed_at")
		if err1 != nil || err2 != nil || err3 != nil {
			return fmt.Errorf("%s/%s: needs members, members_hash and sealed_at", CSeal, id)
		}
		switch {
		case int(n) != len(rows[id]):
			return fmt.Errorf("snapshot %s: its seal covers %d members but %d are stored; a sealed snapshot can't be read until this is resolved", id, n, len(rows[id]))
		case h != sealHash(rows[id]):
			return fmt.Errorf("snapshot %s: a member changed since it was sealed (members_hash differs); a sealed snapshot can't be read until this is resolved", id)
		}
		b, err := manifestBytes(at, manifests[id])
		if err != nil {
			return err
		}
		r.put(layout.InSnapshot(id, layout.Manifest), b)
	}
	return nil
}

func (r *reader) index(coll string) (map[string]Doc, error) {
	items, err := r.db.List(r.ctx, coll)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Doc, len(items))
	for _, it := range items {
		out[it.ID] = it.Doc
	}
	return out, nil
}

func (r *reader) checkMember(m Member, d Doc) error {
	for k, want := range map[string]string{"snapshot_id": m.Snapshot, "role": m.Role, "account": m.Account, "instrument_id": m.Instrument} {
		if v, err := str(d, k); err != nil || v != want {
			return fmt.Errorf("%s/%s: field %s doesn't match the id", CMember, m.ID(), k)
		}
	}
	return nil
}

// body reassembles and checks one raw_response.
func (r *reader) body(sha string) ([]byte, error) {
	d, ok := r.bodies[sha]
	if !ok {
		return nil, fmt.Errorf("raw_response %s: %w", sha, fs.ErrNotExist)
	}
	return readBody(r.ctx, r.db, sha, d)
}

func readBody(ctx context.Context, db DB, sha string, d Doc) ([]byte, error) {
	n, err1 := i64(d, "chunks")
	size, err2 := i64(d, "bytes")
	if err1 != nil || err2 != nil {
		return nil, fmt.Errorf("raw_response %s: needs chunks and bytes", sha)
	}
	chunks, err := db.List(ctx, CRawResponse+"/"+sha+"/"+CChunk)
	if err != nil {
		return nil, err
	}
	if int64(len(chunks)) != n {
		return nil, fmt.Errorf("raw_response %s: says %d chunks, has %d", sha, n, len(chunks))
	}
	var buf bytes.Buffer
	for i, c := range chunks {
		if c.ID != fmt.Sprintf("%04d", i) {
			return nil, fmt.Errorf("raw_response %s: chunk %s out of sequence", sha, c.ID)
		}
		b, err := byts(c.Doc, "body")
		if err != nil {
			return nil, fmt.Errorf("raw_response %s chunk %s: %w", sha, c.ID, err)
		}
		buf.Write(b)
	}
	if int64(buf.Len()) != size || sum(buf.Bytes()) != sha {
		return nil, fmt.Errorf("raw_response %s: stored bytes don't hash to the id", sha)
	}
	return buf.Bytes(), nil
}

func (r *reader) loadLogs() error {
	r.chains, r.lines = map[string][]string{}, map[string][][]byte{}
	items, err := r.db.List(r.ctx, COrderVersion)
	if err != nil {
		return err
	}
	// Ids sort by account, then by zero-padded seq.
	for _, it := range items {
		acct, _, _ := strings.Cut(it.ID, "|")
		seq := int64(len(r.lines[acct]) + 1)
		if it.ID != orderVersionID(acct, seq) {
			return fmt.Errorf("%s/%s: expected %s; the %s log has a gap or is out of order", COrderVersion, it.ID, orderVersionID(acct, seq), acct)
		}
		line, row, err := checkVersion(it.Doc, acct, seq, last(r.chains[acct]))
		if err != nil {
			return fmt.Errorf("%s/%s: %w", COrderVersion, it.ID, err)
		}
		r.lines[acct] = append(r.lines[acct], line)
		r.chains[acct] = append(r.chains[acct], row)
	}
	for acct, lines := range r.lines {
		var b bytes.Buffer
		for _, l := range lines {
			b.Write(l)
			b.WriteByte('\n')
		}
		r.put(LogPath(acct), b.Bytes())
	}
	return nil
}

func last(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[len(s)-1]
}

// checkVersion rebuilds one log line and checks its fields and its link in
// the hash chain.
func checkVersion(d Doc, acct string, seq int64, prev string) ([]byte, string, error) {
	obs, err1 := str(d, "observed_at")
	oj, err2 := str(d, "order_json")
	if err1 != nil || err2 != nil {
		return nil, "", fmt.Errorf("needs observed_at and order_json")
	}
	if a, err := str(d, "account"); err != nil || a != acct {
		return nil, "", fmt.Errorf("account field doesn't match the id")
	}
	if s, err := i64(d, "seq"); err != nil || s != seq {
		return nil, "", fmt.Errorf("seq field doesn't match the id")
	}
	line, err := logLine(obs, oj)
	if err != nil {
		return nil, "", err
	}
	src, _ := d["broker"].(string) // absent: Robinhood
	h, err := history.OrderHeadFor([]byte(oj), src)
	if err != nil {
		return nil, "", err
	}
	for k, want := range map[string]string{"order_id": h.ID, "state": h.State, "created_at": h.CreatedAt, "version_key": h.VersionKey} {
		if v, err := str(d, k); err != nil || v != want {
			return nil, "", fmt.Errorf("field %s doesn't match order_json", k)
		}
	}
	if p, err := str(d, "prev_hash"); err != nil || p != prev {
		return nil, "", fmt.Errorf("prev_hash breaks the chain")
	}
	row := chainHash(prev, line)
	if rh, err := str(d, "row_hash"); err != nil || rh != row {
		return nil, "", fmt.Errorf("row_hash doesn't match the line")
	}
	return line, row, nil
}

func prefixHash(lines [][]byte) string {
	var b bytes.Buffer
	for _, l := range lines {
		b.Write(l)
		b.WriteByte('\n')
	}
	return sum(b.Bytes())
}

func (r *reader) loadExpirations() error {
	items, err := r.db.List(r.ctx, CExpiration)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return nil
	}
	type rec struct {
		n int64
		e ledger.Expiration
	}
	var recs []rec
	for _, it := range items {
		var e ledger.Expiration
		var err error
		get := func(k string) string {
			v, gerr := str(it.Doc, k)
			if gerr != nil && err == nil {
				err = gerr
			}
			return v
		}
		e.Contract, e.Symbol, e.Type, e.Expiry, e.Outcome, e.Source = get("contract"), get("symbol"), get("type"), get("expiry"), get("outcome"), get("source")
		strike, close := get("strike"), get("underlying_close")
		bar := get("bar_sha256")
		n, nerr := i64(it.Doc, "ordinal")
		if err == nil {
			err = nerr
		}
		if err == nil {
			e.Strike, err = decimal.NewFromString(strike)
		}
		if err == nil {
			e.Close, err = decimal.NewFromString(close)
		}
		if err == nil && it.ID != expirationID(e) {
			err = fmt.Errorf("id doesn't match symbol|type|strike|expiry")
		}
		if err == nil && bar != "" {
			if _, ok := r.bodies[bar]; !ok {
				err = fmt.Errorf("bar_sha256 %s isn't a stored raw_response", bar)
			}
		}
		if err != nil {
			return fmt.Errorf("%s/%s: %w", CExpiration, it.ID, err)
		}
		recs = append(recs, rec{n, e})
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].n < recs[j].n })
	cs := make([]ledger.Expiration, len(recs))
	for i, r := range recs {
		if r.n != int64(i+1) {
			return fmt.Errorf("%s: ordinals must run 1..%d without gaps", CExpiration, len(recs))
		}
		cs[i] = r.e
	}
	b, err := expirationsFile(cs)
	if err != nil {
		return err
	}
	// The same check the files Source gets: outcomes must follow from the closes.
	if _, err := ledger.ParseExpirations(snapshot.ExpirationsFile, b); err != nil {
		return err
	}
	r.put(snapshot.ExpirationsFile, b)
	return nil
}
