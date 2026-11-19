package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

// The DB contract is run on every backend that is available: Mem always, and
// PostgreSQL when POSTGRES_DSN names a database the test may create schemas in
// (a local PostgreSQL 16 is enough; see docs/cloud-sql-design.md).

func TestDBContract(t *testing.T) {
	t.Run("memory", func(t *testing.T) { dbContract(t, NewMem()) })
	t.Run("postgres", func(t *testing.T) { dbContract(t, scratchDB(t)) })
}

// scratchDB is a fresh PostgreSQL store, or a skipped test without POSTGRES_DSN.
func scratchDB(t *testing.T) *Postgres {
	t.Helper()
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_DSN not set: the postgres backend is skipped")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	p, cleanup, err := ScratchPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	return p
}

func dbContract(t *testing.T, db DB) {
	ctx := context.Background()

	t.Run("create then get, every value type", func(t *testing.T) {
		in := Doc{
			"text":     "héllo \"quoted\" \\ \n tab\t",
			"empty":    "",
			"n":        int64(42),
			"zero":     int64(0),
			"min":      int64(math.MinInt64),
			"max":      int64(math.MaxInt64),
			"yes":      true,
			"no":       false,
			"bytes":    []byte{0, 1, 2, 0xff, 0xfe},
			"nobytes":  []byte{},
			"money":    "-76.3333",
			"asnumber": "123456789012345678901234567890",
		}
		if err := db.Create(ctx, "rt", "doc1", in); err != nil {
			t.Fatal(err)
		}
		got, err := db.Get(ctx, "rt", "doc1")
		if err != nil {
			t.Fatal(err)
		}
		if !sameDoc(got, in) {
			t.Fatalf("round trip changed the document:\n in  %#v\n got %#v", in, got)
		}
		for k, want := range in {
			switch want.(type) {
			case int64:
				if _, ok := got[k].(int64); !ok {
					t.Errorf("%s came back as %T, want int64", k, got[k])
				}
			case []byte:
				if _, ok := got[k].([]byte); !ok {
					t.Errorf("%s came back as %T, want []byte", k, got[k])
				}
			}
		}
	})

	t.Run("a large byte value", func(t *testing.T) {
		big := bytes.Repeat([]byte{0, 1, 2, 3, 250, 251}, 80<<10) // ~480 KiB, as a chunk is
		if err := db.Create(ctx, "big", "c1", Doc{"data": big}); err != nil {
			t.Fatal(err)
		}
		got, err := db.Get(ctx, "big", "c1")
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got["data"].([]byte), big) {
			t.Fatal("large bytes changed")
		}
	})

	t.Run("create twice is ErrExists and keeps the first", func(t *testing.T) {
		if err := db.Create(ctx, "dup", "a", Doc{"v": "first"}); err != nil {
			t.Fatal(err)
		}
		err := db.Create(ctx, "dup", "a", Doc{"v": "second"})
		if !errors.Is(err, ErrExists) {
			t.Fatalf("second create: %v, want ErrExists", err)
		}
		got, _ := db.Get(ctx, "dup", "a")
		if got["v"] != "first" {
			t.Fatalf("the stored document changed to %v", got["v"])
		}
	})

	t.Run("the same id in two collections is two documents", func(t *testing.T) {
		if err := db.Create(ctx, "c1", "same", Doc{"v": "one"}); err != nil {
			t.Fatal(err)
		}
		if err := db.Create(ctx, "c2", "same", Doc{"v": "two"}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("get a missing document", func(t *testing.T) {
		_, err := db.Get(ctx, "nowhere", "nothing")
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("got %v, want fs.ErrNotExist", err)
		}
	})

	t.Run("list is sorted by byte order", func(t *testing.T) {
		ids := []string{"b", "B", "a", "_x", "10", "9", "é", "z", "A1", "a1", "~", "0"}
		for _, id := range ids {
			if err := db.Create(ctx, "order", id, Doc{"id": id}); err != nil {
				t.Fatal(err)
			}
		}
		items, err := db.List(ctx, "order")
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"0", "10", "9", "A1", "B", "_x", "a", "a1", "b", "z", "~", "é"}
		var got []string
		for _, it := range items {
			got = append(got, it.ID)
			if it.Doc["id"] != it.ID {
				t.Errorf("%s listed with the document of %v", it.ID, it.Doc["id"])
			}
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("order %v, want %v", got, want)
		}
	})

	t.Run("list keeps subcollections apart", func(t *testing.T) {
		db.Create(ctx, "raw/abc/chunk", "0001", Doc{"n": int64(1)})
		db.Create(ctx, "raw/abc/chunk", "0002", Doc{"n": int64(2)})
		db.Create(ctx, "raw/abd/chunk", "0001", Doc{"n": int64(3)})
		items, err := db.List(ctx, "raw/abc/chunk")
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 2 || items[0].ID != "0001" || items[1].ID != "0002" {
			t.Fatalf("got %v", items)
		}
	})

	t.Run("list an empty collection", func(t *testing.T) {
		items, err := db.List(ctx, "empty")
		if err != nil || len(items) != 0 {
			t.Fatalf("got %v, %v", items, err)
		}
	})

	t.Run("a stored document is not changed by the caller's copy", func(t *testing.T) {
		in := Doc{"b": []byte{1, 2, 3}}
		db.Create(ctx, "alias", "x", in)
		in["b"].([]byte)[0] = 9
		got, _ := db.Get(ctx, "alias", "x")
		if got["b"].([]byte)[0] != 1 {
			t.Fatal("the stored bytes alias the caller's slice")
		}
	})

	t.Run("refused input", func(t *testing.T) {
		bad := []struct {
			name, coll, id string
			doc            Doc
		}{
			{"NUL in a value", "g", "g1", Doc{"v": "a\x00b"}},
			{"NUL in a field name", "g", "g2", Doc{"a\x00b": "v"}},
			{"NUL in an id", "g", "g3\x00", Doc{"v": "x"}},
			{"NUL in a collection", "g\x00", "g4", Doc{"v": "x"}},
			{"invalid UTF-8 in a value", "g", "g5", Doc{"v": "a\xffb"}},
			{"invalid UTF-8 in an id", "g", "g6\xff", Doc{"v": "x"}},
			{"unsupported type", "g", "g7", Doc{"v": 1.5}},
			{"empty id", "g", "", Doc{"v": "x"}},
			{"slash in an id", "g", "a/b", Doc{"v": "x"}},
			{"over the size limit", "g", "g8", Doc{"v": strings.Repeat("x", MaxDocBytes+1)}},
		}
		for _, c := range bad {
			if err := db.Create(ctx, c.coll, c.id, c.doc); err == nil {
				t.Errorf("%s: accepted", c.name)
			}
		}
		items, _ := db.List(ctx, "g")
		if len(items) != 0 {
			t.Errorf("a refused document was stored: %v", items)
		}
	})
}

func TestPostgresAppendOnly(t *testing.T) {
	p := scratchDB(t)
	ctx := context.Background()
	if err := p.Create(ctx, "ao", "x", Doc{"v": "original"}); err != nil {
		t.Fatal(err)
	}

	// The owner is refused too: the trigger applies to everyone.
	for _, stmt := range []string{
		`UPDATE doc SET fields = '{}'`,
		`DELETE FROM doc`,
		`TRUNCATE doc`,
	} {
		if _, err := p.pool.Exec(ctx, stmt); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s as the owner: %v, want an append-only refusal", stmt, err)
		}
	}
	got, err := p.Get(ctx, "ao", "x")
	if err != nil || got["v"] != "original" {
		t.Fatalf("the document did not survive: %v, %v", got, err)
	}

	// The server's user is granted SELECT and INSERT and nothing else.
	role := fmt.Sprintf("ob_test_srv_%d", time.Now().UnixNano())
	if _, err := p.pool.Exec(ctx, "CREATE ROLE "+role+" NOLOGIN"); err != nil {
		t.Skipf("cannot create a role here (%v); the grant test is skipped", err)
	}
	t.Cleanup(func() {
		p.pool.Exec(context.Background(), "DROP OWNED BY "+role)
		p.pool.Exec(context.Background(), "DROP ROLE IF EXISTS "+role)
	})
	if err := p.Migrate(ctx, role); err != nil {
		t.Fatal(err)
	}
	var schema string
	if err := p.pool.QueryRow(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if _, err := p.pool.Exec(ctx, `GRANT USAGE ON SCHEMA "`+schema+`" TO `+role); err != nil {
		t.Fatal(err)
	}

	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET ROLE "+role); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(context.Background(), "RESET ROLE")

	if _, err := conn.Exec(ctx, `INSERT INTO doc (coll, id, fields) VALUES ('ao', 'y', '{}')`); err != nil {
		t.Errorf("the server's user cannot insert: %v", err)
	}
	if _, err := conn.Exec(ctx, `SELECT 1 FROM doc LIMIT 1`); err != nil {
		t.Errorf("the server's user cannot select: %v", err)
	}
	for _, stmt := range []string{
		`UPDATE doc SET fields = '{}'`,
		`DELETE FROM doc`,
		`TRUNCATE doc`,
		`DROP TABLE doc`,
		`ALTER TABLE doc DISABLE TRIGGER ALL`,
	} {
		if _, err := conn.Exec(ctx, stmt); err == nil {
			t.Errorf("%s as the server's user was allowed", stmt)
		}
	}
}

func TestPostgresMigrateIsRepeatable(t *testing.T) {
	p := scratchDB(t)
	ctx := context.Background()
	if err := p.Create(ctx, "m", "x", Doc{"v": "kept"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := p.Migrate(ctx, ""); err != nil {
			t.Fatalf("migrate %d: %v", i+1, err)
		}
	}
	got, err := p.Get(ctx, "m", "x")
	if err != nil || got["v"] != "kept" {
		t.Fatalf("migrating again changed the data: %v, %v", got, err)
	}
	// And the trigger is still there after the repeat.
	if _, err := p.pool.Exec(ctx, `DELETE FROM doc`); err == nil {
		t.Fatal("DELETE allowed after a repeated migration")
	}
}

// The design doc's open question, jsonb or exact bytes: a store imported into
// PostgreSQL must verify and export byte for byte what the same repo exports
// from memory, and a store rebuilt from that export must export it again.
func TestPostgresExportMatchesMemory(t *testing.T) {
	p := scratchDB(t)
	ctx := context.Background()
	mem := NewMem()
	importRepo(t, mem)
	importRepo(t, p)
	if _, err := Verify(ctx, p); err != nil {
		t.Fatalf("verify on postgres: %v", err)
	}
	want, got := map[string][]byte{}, map[string][]byte{}
	if err := Export(ctx, mem, mapSink(want)); err != nil {
		t.Fatal(err)
	}
	if err := Export(ctx, p, mapSink(got)); err != nil {
		t.Fatal(err)
	}
	if err := sameExport(want, got); err != nil {
		t.Fatalf("postgres export differs from memory: %v", err)
	}

	again := scratchDB(t)
	if err := ImportExport(ctx, again, exportFS(got)); err != nil {
		t.Fatal(err)
	}
	third := map[string][]byte{}
	if err := Export(ctx, again, mapSink(third)); err != nil {
		t.Fatal(err)
	}
	if err := sameExport(want, third); err != nil {
		t.Fatalf("export, import, export on postgres: %v", err)
	}
	t.Logf("%d export files identical", len(want))
}
