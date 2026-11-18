// Package storetest gives tests every Source the repo's data can be read
// from, so each acceptance test runs once per source (the storage design phase 2:
// "every acceptance test passes on both sources"):
//
//	files      the repo on disk, as the tools read it today
//	memory     the repo imported into the in-memory store and read back
//	emulator   the repo imported into the Firestore emulator and read back;
//	           only when FIRESTORE_EMULATOR_HOST is set (see README)
package storetest

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

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
	emuOnce sync.Once
	emuFS   fs.FS
	emuErr  error
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

	if host := os.Getenv("FIRESTORE_EMULATOR_HOST"); host != "" {
		emuOnce.Do(func() {
			// A fresh project per test binary, so runs never see each other's data.
			db := store.NewEmulator(host, fmt.Sprintf("options-book-test-%d-%d", time.Now().UnixNano(), os.Getpid()))
			emuFS, emuErr = imported(db, disk)
		})
		if emuErr != nil {
			t.Fatalf("emulator source: %v", emuErr)
		}
		out = append(out, Source{Name: "emulator", FS: emuFS})
	} else {
		t.Log("FIRESTORE_EMULATOR_HOST not set: the emulator source is skipped")
	}
	return out
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
