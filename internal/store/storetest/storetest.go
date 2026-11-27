// Package storetest gives tests every Source the repo's data can be read
// from, so each acceptance test runs once per source (the storage design phase 2:
// "every acceptance test passes on both sources"):
//
//	files      the repo on disk, as the tools read it today
//	memory     the repo imported into the in-memory store and read back
//	postgres   the repo imported into a scratch schema of a PostgreSQL database
//	           and read back; only when POSTGRES_DSN is set (see README)
package storetest

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/branonm/options-book/internal/store"
)

// Source is one way of reading the repo layout.
type Source struct {
	Name string
	FS   fs.FS
}

// ModuleRoot is the repository root (the directory holding go.mod), found from
// the test's working directory.
func ModuleRoot(t testing.TB) string {
	t.Helper()
	d, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
		up := filepath.Dir(d)
		if up == d {
			t.Fatal("no go.mod above the test directory")
		}
		d = up
	}
}

// Root is the repo root the tests read their data from: the synthetic one in
// testdata/synthetic. OB_TEST_ROOT=<absolute dir>
// overrides it.
func Root(t testing.TB) string {
	t.Helper()
	if d := os.Getenv("OB_TEST_ROOT"); d != "" {
		abs, err := filepath.Abs(d)
		if err != nil {
			t.Fatal(err)
		}
		return abs
	}
	return DataRoot(ModuleRoot(t))
}

var (
	memOnce sync.Once
	memFS   fs.FS
	memErr  error
	pgOnce  sync.Once
	pgFS    fs.FS
	pgErr   error
)

// Sources returns every available source. The imported ones are built once
// per test binary.
func Sources(t testing.TB) []Source {
	t.Helper()
	root := Root(t)
	disk := os.DirFS(root)
	out := []Source{{Name: "files", FS: disk}}

	memOnce.Do(func() { memFS, memErr = imported(store.NewMem(), disk) })
	if memErr != nil {
		t.Fatalf("memory source: %v", memErr)
	}
	out = append(out, Source{Name: "memory", FS: memFS})

	if dsn := os.Getenv("POSTGRES_DSN"); dsn != "" {
		pgOnce.Do(func() { pgFS, pgErr = importedPostgres(dsn, disk) })
		if pgErr != nil {
			t.Fatalf("postgres source: %v", pgErr)
		}
		out = append(out, Source{Name: "postgres", FS: pgFS})
	} else {
		t.Log("POSTGRES_DSN not set: the postgres source is skipped")
	}
	return out
}

// importedPostgres imports the repo into a scratch schema of the database the
// DSN names and reads it back. The schema is left for the next run's cleanup
// (store.ScratchPostgres removes any older than an hour), because nothing here
// knows when the last test in the binary is done with it.
func importedPostgres(dsn string, disk fs.FS) (fs.FS, error) {
	ctx := context.Background()
	db, _, err := store.ScratchPostgres(ctx, dsn)
	if err != nil {
		return nil, err
	}
	return imported(db, disk)
}

func imported(db store.DB, disk fs.FS) (fs.FS, error) {
	ctx := context.Background()
	if _, err := store.ImportRepo(ctx, db, disk); err != nil {
		return nil, err
	}
	src, err := store.Open(ctx, db)
	if err != nil {
		return nil, err
	}
	return src.FS, nil
}
