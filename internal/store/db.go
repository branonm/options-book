// Package store is the Firestore side of phase 2 (the storage design,
// "Data model (v1, Firestore)"): the collections, an importer from today's
// file layout, a Source that presents the same layout read back from the
// documents, and export / import / verify.
//
// Every write is a create. The document id is the key, so writing an id that
// exists fails (ErrExists); that is how uniqueness and append-only are
// enforced, and in production the service account's IAM role has no update or
// delete permission at all. Nothing in this package updates or deletes.
package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

// Doc is one document's fields. Values are string, int64, bool or []byte
// only: money is always a string, exactly as Robinhood sent it, never a
// Firestore number (those are floating point).
type Doc map[string]any

// Item is a listed document.
type Item struct {
	ID  string
	Doc Doc
}

// ErrExists is returned by Create when the document id is already taken.
var ErrExists = errors.New("document already exists")

// DB is a create-only document store. Collection paths are slash-separated
// and may name a subcollection ("raw_response/<sha256>/chunk").
type DB interface {
	// Create writes a new document, or returns ErrExists.
	Create(ctx context.Context, coll, id string, d Doc) error
	// Get returns a document, or an error wrapping fs.ErrNotExist.
	Get(ctx context.Context, coll, id string) (Doc, error)
	// List returns every document in a collection, sorted by id.
	List(ctx context.Context, coll string) ([]Item, error)
}

// Mem is an in-memory DB, for tests and for export → import → export checks.
type Mem struct {
	mu   sync.Mutex
	docs map[string]map[string]Doc
}

func NewMem() *Mem { return &Mem{docs: map[string]map[string]Doc{}} }

func (m *Mem) Create(_ context.Context, coll, id string, d Doc) error {
	if err := checkID(coll, id); err != nil {
		return err
	}
	if err := checkDoc(d); err != nil {
		return fmt.Errorf("%s/%s: %w", coll, id, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.docs[coll]
	if c == nil {
		c = map[string]Doc{}
		m.docs[coll] = c
	}
	if _, ok := c[id]; ok {
		return fmt.Errorf("%s/%s: %w", coll, id, ErrExists)
	}
	c[id] = copyDoc(d)
	return nil
}

func (m *Mem) Get(_ context.Context, coll, id string) (Doc, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.docs[coll][id]
	if !ok {
		return nil, fmt.Errorf("%s/%s: %w", coll, id, fs.ErrNotExist)
	}
	return copyDoc(d), nil
}

func (m *Mem) List(_ context.Context, coll string) ([]Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Item, 0, len(m.docs[coll]))
	for id, d := range m.docs[coll] {
		out = append(out, Item{ID: id, Doc: copyDoc(d)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// checkID applies Firestore's document-id rules, so the in-memory store
// refuses what the real one would.
func checkID(coll, id string) error {
	if id == "" || id == "." || id == ".." || strings.Contains(id, "/") ||
		(strings.HasPrefix(id, "__") && strings.HasSuffix(id, "__")) || len(id) > 1500 {
		return fmt.Errorf("%s: invalid document id %q", coll, id)
	}
	if err := checkText(coll); err != nil {
		return fmt.Errorf("collection %q: %w", coll, err)
	}
	if err := checkText(id); err != nil {
		return fmt.Errorf("%s: document id %q: %w", coll, id, err)
	}
	return nil
}

// checkText refuses what PostgreSQL's text and jsonb cannot hold: a NUL, and
// bytes that are not valid UTF-8. Every backend refuses it, so a value that
// works in memory cannot fail only once it reaches the real store.
func checkText(s string) error {
	if strings.IndexByte(s, 0) >= 0 {
		return errors.New("contains a NUL character")
	}
	if !utf8.ValidString(s) {
		return errors.New("is not valid UTF-8")
	}
	return nil
}

// MaxDocBytes is a conservative bound under Firestore's 1 MiB document cap.
const MaxDocBytes = 1000 << 10

func checkDoc(d Doc) error {
	n := 0
	for k, v := range d {
		n += len(k) + 1
		if err := checkText(k); err != nil {
			return fmt.Errorf("field name %q: %w", k, err)
		}
		switch v := v.(type) {
		case string:
			if err := checkText(v); err != nil {
				return fmt.Errorf("field %s: %w", k, err)
			}
			n += len(v) + 1
		case []byte:
			n += len(v)
		case int64, bool:
			n += 8
		default:
			return fmt.Errorf("field %s: unsupported type %T", k, v)
		}
	}
	if n > MaxDocBytes {
		return fmt.Errorf("document is %d bytes, over the %d limit", n, MaxDocBytes)
	}
	return nil
}

func copyDoc(d Doc) Doc {
	out := make(Doc, len(d))
	for k, v := range d {
		if b, ok := v.([]byte); ok {
			v = bytes.Clone(b)
		}
		out[k] = v
	}
	return out
}

// sameDoc reports whether two documents hold identical fields.
func sameDoc(a, b Doc) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok {
			return false
		}
		switch av := av.(type) {
		case []byte:
			bb, ok := bv.([]byte)
			if !ok || !bytes.Equal(av, bb) {
				return false
			}
		default:
			if av != bv {
				return false
			}
		}
	}
	return true
}

// createOrSame creates a document, or accepts an existing one with identical
// fields, so an interrupted import can be re-run. A differing document is an
// error: nothing stored is ever changed.
func createOrSame(ctx context.Context, db DB, coll, id string, d Doc) error {
	err := db.Create(ctx, coll, id, d)
	if !errors.Is(err, ErrExists) {
		return err
	}
	have, gerr := db.Get(ctx, coll, id)
	if gerr != nil {
		return gerr
	}
	if !sameDoc(have, d) {
		return fmt.Errorf("%s/%s already exists with different contents; stored documents are never changed", coll, id)
	}
	return nil
}

// field accessors: a missing or mistyped field is an error, never a zero value.

func str(d Doc, k string) (string, error) {
	v, ok := d[k].(string)
	if !ok {
		return "", fmt.Errorf("field %s: want string, have %T", k, d[k])
	}
	return v, nil
}

func i64(d Doc, k string) (int64, error) {
	v, ok := d[k].(int64)
	if !ok {
		return 0, fmt.Errorf("field %s: want integer, have %T", k, d[k])
	}
	return v, nil
}

func byts(d Doc, k string) ([]byte, error) {
	v, ok := d[k].([]byte)
	if !ok {
		return nil, fmt.Errorf("field %s: want bytes, have %T", k, d[k])
	}
	return v, nil
}
