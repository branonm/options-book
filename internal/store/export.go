package store

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"testing/fstest"
)

// The export is the storage design's durable copy outside GCP: a deterministic
// text tree that a later import rebuilds the database from exactly.
//
//	bodies/<sha256>        each raw_response body, byte for byte
//	<collection>.jsonl     one line per document, sorted by id:
//	                       {"id":"…","fields":{…}} with keys sorted
//
// Chunk documents are not exported; the body is, and import re-chunks it.
// Append-only changes show up as pure additions between two exports.

// Sink receives exported files by slash path.
type Sink func(name string, b []byte) error

// Export writes the store to sink. It opens the store first, so only a store
// that passes every check is exported.
func Export(ctx context.Context, db DB, sink Sink) error {
	if _, err := Open(ctx, db); err != nil {
		return fmt.Errorf("not exporting a store that fails its checks: %w", err)
	}
	for _, coll := range Collections {
		items, err := db.List(ctx, coll)
		if err != nil {
			return err
		}
		var b bytes.Buffer
		for _, it := range items {
			if coll == CRawResponse {
				body, err := readBody(ctx, db, it.ID, it.Doc)
				if err != nil {
					return err
				}
				if err := sink("bodies/"+it.ID, body); err != nil {
					return err
				}
			}
			line, err := exportLine(it)
			if err != nil {
				return fmt.Errorf("%s/%s: %w", coll, it.ID, err)
			}
			b.Write(line)
			b.WriteByte('\n')
		}
		if err := sink(coll+".jsonl", b.Bytes()); err != nil {
			return err
		}
	}
	return nil
}

func exportLine(it Item) ([]byte, error) {
	for k, v := range it.Doc {
		if _, ok := v.([]byte); ok {
			return nil, fmt.Errorf("field %s: bytes only belong in chunk documents", k)
		}
	}
	// encoding/json sorts map keys, so the line is deterministic.
	return json.Marshal(struct {
		ID     string `json:"id"`
		Fields Doc    `json:"fields"`
	}{it.ID, it.Doc})
}

// ImportExport rebuilds a store from an export (fsys holds bodies/ and the
// .jsonl files). Bodies first, then the rest in an order where nothing is
// written before what it points at, seals last.
func ImportExport(ctx context.Context, db DB, fsys fs.FS) error {
	known := map[string]bool{"bodies": true}
	for _, c := range Collections {
		known[c+".jsonl"] = true
	}
	ents, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return err
	}
	for _, e := range ents {
		if !known[e.Name()] {
			return fmt.Errorf("%s: not part of an export", e.Name())
		}
	}
	order := []string{CRawResponse, COrderVersion, CExpiration, CMember, COrderPos, CSnapshot, CSeal, CRun}
	for _, coll := range order {
		items, err := readExport(fsys, coll)
		if err != nil {
			return err
		}
		for _, it := range items {
			if coll == CRawResponse {
				body, err := fs.ReadFile(fsys, "bodies/"+it.ID)
				if err != nil {
					return err
				}
				if sum(body) != it.ID {
					return fmt.Errorf("bodies/%s: bytes don't hash to the name", it.ID)
				}
				n := 0
				for off := 0; off < len(body) || n == 0; off += ChunkBytes {
					end := min(off+ChunkBytes, len(body))
					if err := createOrSame(ctx, db, CRawResponse+"/"+it.ID+"/"+CChunk, fmt.Sprintf("%04d", n), Doc{"body": body[off:end]}); err != nil {
						return err
					}
					n++
				}
				if c, err := i64(it.Doc, "chunks"); err != nil || c != int64(n) {
					return fmt.Errorf("%s/%s: chunks field doesn't match the body", CRawResponse, it.ID)
				}
			}
			if err := createOrSame(ctx, db, coll, it.ID, it.Doc); err != nil {
				return err
			}
		}
	}
	return nil
}

func readExport(fsys fs.FS, coll string) ([]Item, error) {
	b, err := fs.ReadFile(fsys, coll+".jsonl")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s.jsonl missing from the export", coll)
	}
	if err != nil {
		return nil, err
	}
	var out []Item
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 1<<20), MaxDocBytes*2)
	prev := ""
	for n := 1; sc.Scan(); n++ {
		dec := json.NewDecoder(bytes.NewReader(sc.Bytes()))
		dec.UseNumber()
		dec.DisallowUnknownFields()
		var line struct {
			ID     string         `json:"id"`
			Fields map[string]any `json:"fields"`
		}
		if err := dec.Decode(&line); err != nil {
			return nil, fmt.Errorf("%s.jsonl line %d: %w", coll, n, err)
		}
		if line.ID <= prev {
			return nil, fmt.Errorf("%s.jsonl line %d: ids must be sorted and unique", coll, n)
		}
		prev = line.ID
		d := Doc{}
		for k, v := range line.Fields {
			switch v := v.(type) {
			case string, bool:
				d[k] = v
			case json.Number:
				i, err := v.Int64()
				if err != nil {
					return nil, fmt.Errorf("%s.jsonl line %d field %s: integers only", coll, n, k)
				}
				d[k] = i
			default:
				return nil, fmt.Errorf("%s.jsonl line %d field %s: unsupported value %T", coll, n, k, v)
			}
		}
		out = append(out, Item{ID: line.ID, Doc: d})
	}
	return out, sc.Err()
}

// VerifyReport is what `ob db verify` checked.
type VerifyReport struct {
	Snapshots, Sealed, Bodies, OrderVersions int
	ExportBytes                              int
}

// Verify re-checks the whole store (every body's sha256, both order-log hash
// chains, every position and every seal — all of Open's checks), then that
// export → import → export is byte-identical.
func Verify(ctx context.Context, db DB) (*VerifyReport, error) {
	src, err := Open(ctx, db)
	if err != nil {
		return nil, err
	}
	rep := &VerifyReport{Snapshots: len(src.Snapshots)}
	for _, c := range []struct {
		coll string
		n    *int
	}{{CSeal, &rep.Sealed}, {CRawResponse, &rep.Bodies}, {COrderVersion, &rep.OrderVersions}} {
		items, err := db.List(ctx, c.coll)
		if err != nil {
			return nil, err
		}
		*c.n = len(items)
	}
	first := map[string][]byte{}
	if err := Export(ctx, db, mapSink(first)); err != nil {
		return nil, err
	}
	again := NewMem()
	if err := ImportExport(ctx, again, exportFS(first)); err != nil {
		return nil, fmt.Errorf("re-importing the export: %w", err)
	}
	second := map[string][]byte{}
	if err := Export(ctx, again, mapSink(second)); err != nil {
		return nil, fmt.Errorf("exporting the re-import: %w", err)
	}
	if err := sameExport(first, second); err != nil {
		return nil, fmt.Errorf("export → import → export: %w", err)
	}
	for _, b := range first {
		rep.ExportBytes += len(b)
	}
	return rep, nil
}

func mapSink(m map[string][]byte) Sink {
	return func(name string, b []byte) error {
		if _, dup := m[name]; dup {
			return fmt.Errorf("%s written twice", name)
		}
		m[name] = bytes.Clone(b)
		return nil
	}
}

func sameExport(a, b map[string][]byte) error {
	names := map[string]bool{}
	for k := range a {
		names[k] = true
	}
	for k := range b {
		names[k] = true
	}
	var diffs []string
	for k := range names {
		if !bytes.Equal(a[k], b[k]) {
			diffs = append(diffs, k)
		}
	}
	sort.Strings(diffs)
	if len(diffs) > 0 {
		return fmt.Errorf("differs in %s", strings.Join(diffs, ", "))
	}
	return nil
}

// ExportFS presents an in-memory export as an fs.FS, for ImportExport.
func exportFS(m map[string][]byte) fs.FS {
	out := fstest.MapFS{}
	for k, v := range m {
		out[k] = &fstest.MapFile{Data: v, Mode: 0o444}
	}
	return out
}
