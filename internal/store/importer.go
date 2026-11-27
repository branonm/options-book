package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/branonm/options-book/internal/layout"
	"io/fs"
	"regexp"
	"sort"
	"strings"

	"github.com/branonm/options-book/internal/history"
	"github.com/branonm/options-book/internal/ledger"
	"github.com/branonm/options-book/internal/snapshot"
)

// ImportReport says what an import wrote and what it deliberately left out.
type ImportReport struct {
	Bodies        int      // distinct raw_response documents
	Members       int      // snapshot_member documents
	Snapshots     int      // snapshot documents
	Seals         int      // snapshot_seal documents
	OrderPos      int      // snapshot_order_pos documents
	OrderVersions int      // order_version documents
	Expirations   int      // expiration_outcome documents
	Skipped       []string // files not stored, and why
}

// importNote is the provenance written on every imported raw_response. The
// importer can't know whether a file was verbatim or transcribed (that is
// prose in each snapshot's PROVENANCE.md, which is imported as a member), so
// it marks none verbatim and says where to look.
const importNote = "imported by `ob db import` from %s; whether it was verbatim or transcribed is recorded in that snapshot's PROVENANCE.md (member role provenance)"

// ImportRepo writes today's file layout into db: every snapshots/<id>/ folder,
// history/<acct>/orders.jsonl, history/expirations.json, history/bars/ and
// reconcile/<id>/. fsys is rooted at the repo. Each generated file
// (history.json, manifest.json, the order log, expirations.json) is
// regenerated from the documents and must match the file byte for byte, or
// the import stops: the documents must reproduce every sha256 the files carry.
//
// Re-running an interrupted import is safe: an existing identical document is
// accepted, a differing one is an error.
func ImportRepo(ctx context.Context, db DB, fsys fs.FS) (*ImportReport, error) {
	im := &importer{ctx: ctx, db: db, fsys: fsys, rep: &ImportReport{}, bodies: map[string]bool{}}
	if err := im.run(); err != nil {
		return im.rep, err
	}
	return im.rep, nil
}

type importer struct {
	ctx    context.Context
	db     DB
	fsys   fs.FS
	rep    *ImportReport
	bodies map[string]bool // sha256 already written this run
	// pulls[snapshot][account] → compact order JSON → pull page sha256s
	pulls map[string]map[string]map[string][]string
	// positions, per account, sorted by seq: which snapshot first saw each line
	positions map[string][]posRef
	bars      map[string]string // history/bars path → sha256
}

type posRef struct {
	snap string
	seq  int64
}

type snapFiles struct {
	id      string
	members []memberFile
	refs    map[string][]byte // account → history.json bytes
	mf      []byte            // manifest.json, nil if unsealed
}

type memberFile struct {
	m    Member
	rel  string // path relative to the repo root
	body []byte
}

func (im *importer) run() error {
	im.pulls = map[string]map[string]map[string][]string{}
	im.positions = map[string][]posRef{}
	im.bars = map[string]string{}

	snaps, err := im.readSnapshots()
	if err != nil {
		return err
	}
	if err := im.attachReconcile(snaps); err != nil {
		return err
	}
	// Bodies first, so nothing ever points at a body that isn't there.
	for _, s := range snaps {
		for _, f := range s.members {
			if err := im.putBody(f.body, f.m.Role, f.m.Account, f.rel); err != nil {
				return err
			}
		}
	}
	if err := im.importBars(); err != nil {
		return err
	}
	// Positions are needed to attribute log lines to pulls.
	for _, s := range snaps {
		for acct, b := range s.refs {
			var ref history.Ref
			if err := json.Unmarshal(b, &ref); err != nil {
				return fmt.Errorf("%s: %w", layout.InSnapshot(s.id, layout.AccountFile(acct, layout.HistoryRef)), err)
			}
			im.positions[acct] = append(im.positions[acct], posRef{s.id, int64(ref.Lines)})
		}
	}
	for acct := range im.positions {
		sort.SliceStable(im.positions[acct], func(i, j int) bool { return im.positions[acct][i].seq < im.positions[acct][j].seq })
	}
	chains, err := im.importLogs()
	if err != nil {
		return err
	}
	if err := im.importExpirations(); err != nil {
		return err
	}
	for _, s := range snaps {
		if err := im.importSnapshot(s, chains); err != nil {
			return err
		}
	}
	return nil
}

func (im *importer) readSnapshots() ([]*snapFiles, error) {
	ents, err := fs.ReadDir(im.fsys, layout.SnapshotsDir)
	if err != nil {
		return nil, err
	}
	var out []*snapFiles
	for _, e := range ents {
		if !e.IsDir() {
			im.rep.Skipped = append(im.rep.Skipped, layout.Snapshot(e.Name())+": a repo note, not part of any snapshot")
			continue
		}
		id := e.Name()
		if _, err := snapshot.ParseID(id); err != nil {
			return nil, err
		}
		s := &snapFiles{id: id, refs: map[string][]byte{}}
		dir := layout.Snapshot(id)
		err := fs.WalkDir(im.fsys, dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel := strings.TrimPrefix(p, dir+"/")
			kind, m, err := classify(id, rel)
			if err != nil {
				return err
			}
			b, err := fs.ReadFile(im.fsys, p)
			if err != nil {
				return err
			}
			switch kind {
			case kindGenerated:
				im.rep.Skipped = append(im.rep.Skipped, p+": generated output (a test golden), rebuilt from the data")
			case kindManifest:
				s.mf = b
			case kindHistory:
				s.refs[m.Account] = b
			case kindMember:
				s.members = append(s.members, memberFile{m: m, rel: p, body: b})
				if m.Role == RoleOrdersPull {
					if err := im.indexPull(id, m.Account, p, b); err != nil {
						return err
					}
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// indexPull records which compact order records a pull page carried, so each
// log line can name the page it came from (order_version.pull_sha256).
func (im *importer) indexPull(snap, acct, name string, body []byte) error {
	records, _, err := history.PageRecords(name, body)
	if err != nil {
		return err
	}
	if im.pulls[snap] == nil {
		im.pulls[snap] = map[string]map[string][]string{}
	}
	if im.pulls[snap][acct] == nil {
		im.pulls[snap][acct] = map[string][]string{}
	}
	sha := sum(body)
	for _, raw := range records {
		var c bytes.Buffer
		if err := json.Compact(&c, raw); err != nil {
			return err
		}
		k := c.String()
		im.pulls[snap][acct][k] = append(im.pulls[snap][acct][k], sha)
	}
	return nil
}

func (im *importer) attachReconcile(snaps []*snapFiles) error {
	ents, err := fs.ReadDir(im.fsys, layout.ReconcileDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	byID := map[string]*snapFiles{}
	for _, s := range snaps {
		byID[s.id] = s
	}
	for _, e := range ents {
		s, ok := byID[e.Name()]
		if !e.IsDir() || !ok {
			return fmt.Errorf("%s: reconcile inputs must sit in a folder named for an existing snapshot", layout.ReconcileFolder(e.Name()))
		}
		files, err := fs.ReadDir(im.fsys, layout.ReconcileFolder(e.Name()))
		if err != nil {
			return err
		}
		for _, f := range files {
			acct, ok := strings.CutSuffix(f.Name(), layout.PnLSuffix)
			if f.IsDir() || !ok || acct == "" {
				return fmt.Errorf("%s: not a file the store knows how to hold", layout.ReconcileFolder(e.Name())+"/"+f.Name())
			}
			p := layout.ReconcileFolder(e.Name()) + "/" + f.Name()
			b, err := fs.ReadFile(im.fsys, p)
			if err != nil {
				return err
			}
			s.members = append(s.members, memberFile{m: Member{Snapshot: s.id, Role: RoleRealizedPnL, Account: acct}, rel: p, body: b})
		}
	}
	return nil
}

func (im *importer) putBody(body []byte, role, account, rel string) error {
	sha := sum(body)
	if im.bodies[sha] {
		return nil
	}
	// Chunks before the parent, so a parent never names chunks that aren't there.
	n := 0
	for off := 0; off < len(body) || n == 0; off += ChunkBytes {
		end := min(off+ChunkBytes, len(body))
		if err := createOrSame(im.ctx, im.db, CRawResponse+"/"+sha+"/"+CChunk, fmt.Sprintf("%04d", n), Doc{"body": body[off:end]}); err != nil {
			return err
		}
		n++
	}
	source, call := callFor(role)
	d := Doc{
		"source": source, "call": call, "args": "null", "account": "", "fetched_at": "",
		"verbatim": false, "provenance": fmt.Sprintf(importNote, rel),
		"bytes": int64(len(body)), "chunks": int64(n),
	}
	// A body is content-addressed: an earlier import (or an earlier snapshot in
	// this one) may have stored the same bytes with its own provenance. Keep it.
	if err := im.db.Create(im.ctx, CRawResponse, sha, d); err != nil && !errors.Is(err, ErrExists) {
		return err
	}
	im.bodies[sha] = true
	im.rep.Bodies++
	return nil
}

func (im *importer) importBars() error {
	ents, err := fs.ReadDir(im.fsys, layout.BarsFolder())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range ents {
		p := layout.BarsFolder() + "/" + e.Name()
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			return fmt.Errorf("%s: not a file the store knows how to hold", p)
		}
		b, err := fs.ReadFile(im.fsys, p)
		if err != nil {
			return err
		}
		if err := im.putBody(b, RoleBars, "", p); err != nil {
			return err
		}
		im.bars[p] = sum(b)
	}
	return nil
}

// importLogs writes every history/<acct>/orders.jsonl line as an order_version
// and returns each account's row hashes (index seq-1).
func (im *importer) importLogs() (map[string][]string, error) {
	chains := map[string][]string{}
	ents, err := fs.ReadDir(im.fsys, layout.HistoryDir)
	if err != nil {
		return nil, err
	}
	for _, e := range ents {
		if !e.IsDir() || e.Name() == layout.BarsDir {
			continue
		}
		acct := e.Name()
		p := LogPath(acct)
		l, err := history.LoadFS(im.fsys, p)
		if err != nil {
			return nil, err
		}
		prev := ""
		for i, line := range l.Lines() {
			seq := int64(i + 1)
			e := l.Entries[i]
			regen, err := logLine(e.ObservedAt, string(e.Order))
			if err != nil || !bytes.Equal(regen, line) {
				return nil, fmt.Errorf("%s line %d: can't be regenerated byte for byte from its fields", p, seq)
			}
			h, err := history.OrderHead(e.Order)
			if err != nil {
				return nil, fmt.Errorf("%s line %d: %w", p, seq, err)
			}
			row := chainHash(prev, line)
			d := Doc{
				"account": acct, "seq": seq, "order_id": h.ID, "observed_at": e.ObservedAt,
				"version_key": h.VersionKey, "state": h.State, "created_at": h.CreatedAt,
				"order_json": string(e.Order), "pull_sha256": im.pullFor(acct, seq, string(e.Order)),
				"prev_hash": prev, "row_hash": row,
			}
			if err := createOrSame(im.ctx, im.db, COrderVersion, orderVersionID(acct, seq), d); err != nil {
				return nil, err
			}
			im.rep.OrderVersions++
			chains[acct] = append(chains[acct], row)
			prev = row
		}
	}
	return chains, nil
}

// pullFor names the pull page a log line came from: the page, in the
// snapshot whose `history add` appended the line, that carried exactly this
// order record. "" when no page or more than one page matches (lines from
// before pull pages were kept, or an order present in two pages).
func (im *importer) pullFor(acct string, seq int64, order string) string {
	for _, p := range im.positions[acct] {
		if seq <= p.seq {
			if shas := uniq(im.pulls[p.snap][acct][order]); len(shas) == 1 {
				return shas[0]
			}
			return ""
		}
	}
	return ""
}

func uniq(s []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range s {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

var barsRef = regexp.MustCompile(`\((history/bars/[^)]+\.json)\)`)

func (im *importer) importExpirations() error {
	b, err := fs.ReadFile(im.fsys, snapshot.ExpirationsFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := ledger.ParseExpirations(snapshot.ExpirationsFile, b); err != nil {
		return err
	}
	var f ledger.ExpirationsFile
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	if regen, err := expirationsFile(f.Contracts); err != nil || !bytes.Equal(regen, b) {
		return fmt.Errorf("%s can't be regenerated byte for byte from its records (is its _doc still ledger.ExpirationsDoc?)", snapshot.ExpirationsFile)
	}
	for i, e := range f.Contracts {
		bar := ""
		if m := barsRef.FindStringSubmatch(e.Source); m != nil {
			bar = im.bars[m[1]]
			if bar == "" {
				return fmt.Errorf("%s: %s cites %s, which isn't in history/bars", snapshot.ExpirationsFile, e.Contract, m[1])
			}
		}
		d := Doc{
			"contract": e.Contract, "symbol": e.Symbol, "type": e.Type, "strike": e.Strike.String(),
			"expiry": e.Expiry, "underlying_close": e.Close.String(), "outcome": e.Outcome,
			"source": e.Source, "bar_sha256": bar, "recorded_at": "", "ordinal": int64(i + 1),
		}
		if err := createOrSame(im.ctx, im.db, CExpiration, expirationID(e), d); err != nil {
			return err
		}
		im.rep.Expirations++
	}
	return nil
}

func expirationID(e ledger.Expiration) string {
	return strings.Join([]string{e.Symbol, e.Type, e.Strike.String(), e.Expiry}, "|")
}

func (im *importer) importSnapshot(s *snapFiles, chains map[string][]string) error {
	var rows []string
	takenAt := ""
	manifestFiles := map[string]string{}
	for _, f := range s.members {
		sha := sum(f.body)
		d := Doc{"snapshot_id": s.id, "role": f.m.Role, "account": f.m.Account, "instrument_id": f.m.Instrument, "sha256": sha}
		if err := createOrSame(im.ctx, im.db, CMember, f.m.ID(), d); err != nil {
			return err
		}
		im.rep.Members++
		if sealed(f.m.Role) {
			rows = append(rows, f.m.ID()+"|"+sha)
			manifestFiles[strings.TrimPrefix(f.rel, layout.Snapshot(s.id)+"/")] = sha
		}
	}
	accts := make([]string, 0, len(s.refs))
	for a := range s.refs {
		accts = append(accts, a)
	}
	sort.Strings(accts)
	for _, acct := range accts {
		b := s.refs[acct]
		p := layout.InSnapshot(s.id, layout.AccountFile(acct, layout.HistoryRef))
		var ref history.Ref
		if err := json.Unmarshal(b, &ref); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		if ref.Log != LogPath(acct) {
			return fmt.Errorf("%s: refers to %s, not %s", p, ref.Log, LogPath(acct))
		}
		chain := chains[acct]
		if ref.Lines < 0 || ref.Lines > len(chain) {
			return fmt.Errorf("%s: %d lines, but %s has %d", p, ref.Lines, ref.Log, len(chain))
		}
		pos := Doc{
			"seq": int64(ref.Lines), "chain_hash": chainAt(chain, int64(ref.Lines)),
			"log": ref.Log, "prefix_sha256": ref.SHA256, "observed_at": ref.ObservedAt,
		}
		if regen, err := historyRef(pos); err != nil || !bytes.Equal(regen, b) {
			return fmt.Errorf("%s can't be regenerated byte for byte from snapshot_order_pos", p)
		}
		id := s.id + "|" + acct
		if err := createOrSame(im.ctx, im.db, COrderPos, id, pos); err != nil {
			return err
		}
		im.rep.OrderPos++
		rows = append(rows, fmt.Sprintf("%s|%d|%s", id, ref.Lines, chainAt(chain, int64(ref.Lines))))
		manifestFiles[layout.AccountFile(acct, layout.HistoryRef)] = sum(b)
		if ref.ObservedAt > takenAt {
			takenAt = ref.ObservedAt
		}
	}
	if err := createOrSame(im.ctx, im.db, CSnapshot, s.id, Doc{"taken_at": takenAt, "note": "imported by `ob db import` from snapshots/" + s.id}); err != nil {
		return err
	}
	im.rep.Snapshots++
	if s.mf == nil {
		return nil
	}
	var m snapshot.Manifest
	if err := json.Unmarshal(s.mf, &m); err != nil {
		return fmt.Errorf("%s: %w", layout.InSnapshot(s.id, layout.Manifest), err)
	}
	seal := Doc{"sealed_at": m.CreatedAt, "members": int64(len(rows)), "members_hash": sealHash(rows)}
	regen, err := manifestBytes(m.CreatedAt, manifestFiles)
	if err != nil || !bytes.Equal(regen, s.mf) {
		return fmt.Errorf("%s: the sealed members don't reproduce it byte for byte (a file listed in it is missing, or one not listed is present)", layout.InSnapshot(s.id, layout.Manifest))
	}
	if err := createOrSame(im.ctx, im.db, CSeal, s.id, seal); err != nil {
		return err
	}
	im.rep.Seals++
	return nil
}

// LogPath is an account's order log in the repo layout.
func LogPath(acct string) string { return layout.Log(acct) }

// The four generated files, rebuilt with the same encoders that wrote them.

func logLine(observedAt, orderJSON string) ([]byte, error) {
	return json.Marshal(history.Entry{ObservedAt: observedAt, Order: json.RawMessage(orderJSON)})
}

func historyRef(pos Doc) ([]byte, error) {
	seq, err := i64(pos, "seq")
	if err != nil {
		return nil, err
	}
	var ref history.Ref
	ref.Lines = int(seq)
	for k, dst := range map[string]*string{"log": &ref.Log, "prefix_sha256": &ref.SHA256, "observed_at": &ref.ObservedAt} {
		if *dst, err = str(pos, k); err != nil {
			return nil, err
		}
	}
	b, err := json.MarshalIndent(ref, "", "  ")
	return append(b, '\n'), err
}

func manifestBytes(createdAt string, files map[string]string) ([]byte, error) {
	b, err := json.MarshalIndent(snapshot.Manifest{CreatedAt: createdAt, Files: files}, "", "  ")
	return append(b, '\n'), err
}

func expirationsFile(cs []ledger.Expiration) ([]byte, error) {
	b, err := json.MarshalIndent(ledger.ExpirationsFile{Doc: ledger.ExpirationsDoc, Contracts: cs}, "", "  ")
	return append(b, '\n'), err
}
