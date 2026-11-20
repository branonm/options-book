package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/branonm/options-book/internal/store"
	"github.com/branonm/options-book/internal/store/storetest"
)

// A DSN can carry a password, so a spec is never printed whole.
func TestSpecLabelHidesTheDSN(t *testing.T) {
	for spec, want := range map[string]string{
		"postgres=postgres://user:secret@host/db": "postgres",
		"emulator=localhost:8080":                 "emulator=localhost:8080",
		"firestore=some-project":                  "firestore=some-project",
	} {
		if got := specLabel(spec); got != want {
			t.Errorf("specLabel(%q) = %q, want %q", spec, got, want)
		}
		if strings.Contains(specLabel(spec), "secret") {
			t.Errorf("specLabel(%q) shows the password", spec)
		}
	}
}

func TestDBRefusesTwoStores(t *testing.T) {
	err := runDB([]string{"verify", "--postgres", "x", "--emulator", "y"})
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("got %v, want a refusal naming exactly one store", err)
	}
	if err := runDB([]string{"verify"}); err == nil {
		t.Fatal("no store given: accepted")
	}
}

func TestDBMigrateNeedsPostgres(t *testing.T) {
	db := store.NewMem()
	testDBs["emulator=mem"] = db
	t.Cleanup(func() { delete(testDBs, "emulator=mem") })
	if err := runDB([]string{"migrate", "--emulator", "mem"}); err == nil {
		t.Fatal("migrate on a non-PostgreSQL store was accepted")
	}
}

// ob db against a PostgreSQL store: migrate (again; it is repeatable), import
// the repo, verify, export.
func TestDBPostgresMigrateImportVerifyExport(t *testing.T) {
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_DSN not set: the postgres commands are skipped")
	}
	p, cleanup, err := store.ScratchPostgres(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	testDBs["postgres=scratch"] = p
	t.Cleanup(func() { delete(testDBs, "postgres=scratch") })

	steps := [][]string{
		{"migrate", "--postgres", "scratch"},
		{"import", "--postgres", "scratch", "--from-repo", storetest.Root(t)},
		{"verify", "--postgres", "scratch"},
		{"list", "--postgres", "scratch"},
		{"check-append-only", "--postgres", "scratch"},
	}
	for _, args := range steps {
		if err := runDB(args); err != nil {
			t.Fatalf("ob db %s: %v", strings.Join(args, " "), err)
		}
	}
	out := filepath.Join(t.TempDir(), "export")
	if err := runDB([]string{"export", "--postgres", "scratch", "--to", out}); err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(out)
	if err != nil || len(ents) == 0 {
		t.Fatalf("export wrote nothing: %v", err)
	}
}
