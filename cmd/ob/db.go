package main

// `ob db` and the --source flag: the phase 2 store (internal/store,
// the storage design). These read and write Firestore as whoever runs
// them — the owner's own account on his machine, or the local emulator — never
// a broker, and nothing here updates or deletes a document.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/branonm/options-book/internal/broker"
	"github.com/branonm/options-book/internal/report"
	"github.com/branonm/options-book/internal/snapshot"
	"github.com/branonm/options-book/internal/store"
)

// emulatorProject is the project id `ob db --emulator` and
// `--source emulator=…` use; any id works on the emulator.
const emulatorProject = "options-book"

// openDB parses a store spec: emulator=HOST:PORT or firestore=PROJECT.
// testDBs lets tests point a store spec at an in-memory store. Empty in the binary.
var testDBs = map[string]store.DB{}

func openDB(spec string) (store.DB, error) {
	if db, ok := testDBs[spec]; ok {
		return db, nil
	}
	kind, arg, ok := strings.Cut(spec, "=")
	switch {
	case ok && kind == "emulator" && arg != "":
		return store.NewEmulator(arg, emulatorProject), nil
	case ok && kind == "firestore" && arg != "":
		return store.NewFirestore(context.Background(), arg, "(default)")
	case ok && kind == "postgres" && arg != "":
		return store.NewPostgres(context.Background(), arg, "")
	case ok && kind == "cloudsql" && arg != "":
		// cloudsql=PROJECT:REGION:INSTANCE/DATABASE/IAM_USER
		parts := strings.Split(arg, "/")
		if len(parts) != 3 {
			return nil, fmt.Errorf("cloudsql store: want PROJECT:REGION:INSTANCE/DATABASE/USER")
		}
		return store.NewCloudSQL(context.Background(), parts[0], parts[1], parts[2])
	}
	return nil, fmt.Errorf("--source %q: want files, emulator=HOST:PORT, firestore=PROJECT, postgres=DSN or cloudsql=INSTANCE/DATABASE/USER", specLabel(spec))
}

// specLabel is a store spec as it may be printed: a PostgreSQL DSN can carry a
// password, so only its kind is shown.
func specLabel(spec string) string {
	if kind, _, ok := strings.Cut(spec, "="); ok && kind == "postgres" {
		return "postgres"
	}
	return spec
}

var opened = map[string]*store.Source{}

// openSource reads and verifies a store once per run (diff loads two
// snapshots from it).
func openSource(spec string) (*store.Source, error) {
	if s, ok := opened[spec]; ok {
		return s, nil
	}
	db, err := openDB(spec)
	if err != nil {
		return nil, err
	}
	s, err := store.Open(context.Background(), db)
	if err != nil {
		return nil, err
	}
	opened[spec] = s
	return s, nil
}

// atFor locates --snapshot in --source. With a store, the snapshot is named by
// its id; a folder path is accepted and its base name used.
func atFor(source, snap string) (snapshot.At, error) {
	if source == "" || source == "files" {
		return snapshot.OnDisk(snap), nil
	}
	s, err := openSource(source)
	if err != nil {
		return snapshot.At{}, err
	}
	id := filepath.Base(filepath.Clean(snap))
	for _, have := range s.Snapshots {
		if have == id {
			return s.At(id), nil
		}
	}
	return snapshot.At{}, fmt.Errorf("snapshot %s is not in %s", id, source)
}

// filesOnly refuses --source for the commands that write.
func filesOnly(source, cmd string) error {
	if source != "" && source != "files" {
		return fmt.Errorf("wheel %s writes files and runs on --source files only; writing to the store is phase 4", cmd)
	}
	return nil
}

// loadPnL reads a realized-P&L response: a file path on the files source, or
// its repo-layout path (reconcile/<id>/<acct>_realized_pnl.json) in a store.
func loadPnL(source, file string) (broker.RealizedPnL, error) {
	if source == "" || source == "files" {
		b, err := os.ReadFile(file)
		if err != nil {
			return broker.RealizedPnL{}, err
		}
		return snapshot.DecodeRealizedPnL(file, b)
	}
	s, err := openSource(source)
	if err != nil {
		return broker.RealizedPnL{}, err
	}
	name := filepath.ToSlash(filepath.Clean(file))
	b, err := fs.ReadFile(s.FS, name)
	if err != nil {
		return broker.RealizedPnL{}, err
	}
	return snapshot.DecodeRealizedPnL(name, b)
}

func runDB(args []string) error {
	if len(args) == 0 {
		usage()
	}
	sub := args[0]
	fset := flag.NewFlagSet("db "+sub, flag.ExitOnError)
	emulator := fset.String("emulator", "", "local Firestore emulator, HOST:PORT")
	project := fset.String("project", "", "live GCP project (uses your Application Default Credentials)")
	postgres := fset.String("postgres", "", "a PostgreSQL store, as a DSN (it may carry a password; it is never printed)")
	cloudsql := fset.String("cloudsql", "", "a Cloud SQL store, PROJECT:REGION:INSTANCE/DATABASE/IAM_USER (IAM sign-in through your Application Default Credentials)")
	serverRole := fset.String("server-role", "", "migrate: the database role to grant SELECT and INSERT on the store, and nothing else")
	fromRepo := fset.String("from-repo", "", "import: the repo root to import today's files from")
	fromExport := fset.String("from-export", "", "import: a directory written by `ob db export`")
	to := fset.String("to", "", "export: an empty or new directory")
	snap := fset.String("snapshot", "", "show: the snapshot id")
	var raw pullFlags
	fset.Var(&raw, "raw", "show: a file inside the snapshot to print verbatim (repeatable)")
	fset.Parse(args[1:])

	var spec string
	given := 0
	for _, f := range []struct{ flag, kind string }{
		{*emulator, "emulator"}, {*project, "firestore"}, {*postgres, "postgres"}, {*cloudsql, "cloudsql"},
	} {
		if f.flag != "" {
			given++
			spec = f.kind + "=" + f.flag
		}
	}
	if given != 1 {
		return fmt.Errorf("ob db %s: give exactly one of --emulator HOST:PORT, --project ID, --postgres DSN or --cloudsql INSTANCE/DATABASE/USER", sub)
	}
	db, err := openDB(spec)
	if err != nil {
		return err
	}
	label := specLabel(spec)
	ctx := context.Background()

	switch sub {
	case "check-append-only":
		pg, ok := db.(*store.Postgres)
		if !ok {
			return fmt.Errorf("ob db check-append-only: only a PostgreSQL store (--postgres or --cloudsql) has grants to check")
		}
		if err := pg.CheckAppendOnly(ctx); err != nil {
			return err
		}
		fmt.Printf("%s refuses UPDATE, DELETE and TRUNCATE for the connected user (each was tried in a transaction that was rolled back).\n", label)
		return nil

	case "migrate":
		pg, ok := db.(*store.Postgres)
		if !ok {
			return fmt.Errorf("ob db migrate: only a PostgreSQL store (--postgres or --cloudsql) has a schema")
		}
		if err := pg.Migrate(ctx, *serverRole); err != nil {
			return err
		}
		fmt.Printf("Migrated %s: the doc table, and the trigger that refuses UPDATE, DELETE and TRUNCATE.\n", label)
		if *serverRole != "" {
			fmt.Printf("Granted %s SELECT and INSERT on it, and nothing else.\n", *serverRole)
		}
		return nil

	case "import":
		switch {
		case *fromRepo != "" && *fromExport == "":
			rep, err := store.ImportRepo(ctx, db, os.DirFS(*fromRepo))
			if err != nil {
				return err
			}
			fmt.Printf("Imported %s into %s: %d bodies, %d members, %d snapshots (%d sealed), %d order positions, %d order versions, %d expiration outcomes.\n",
				*fromRepo, label, rep.Bodies, rep.Members, rep.Snapshots, rep.Seals, rep.OrderPos, rep.OrderVersions, rep.Expirations)
			for _, s := range rep.Skipped {
				fmt.Println("  not stored:", s)
			}
		case *fromExport != "" && *fromRepo == "":
			if err := store.ImportExport(ctx, db, os.DirFS(*fromExport)); err != nil {
				return err
			}
			fmt.Printf("Imported %s into %s.\n", *fromExport, label)
		default:
			return fmt.Errorf("ob db import: give exactly one of --from-repo or --from-export")
		}
		// Every import ends with the full check.
		rep, err := store.Verify(ctx, db)
		if err != nil {
			return fmt.Errorf("imported, but verify fails: %w", err)
		}
		fmt.Printf("Verified: %d snapshots (%d sealed), %d bodies, %d order versions; export → import → export is byte-identical.\n", rep.Snapshots, rep.Sealed, rep.Bodies, rep.OrderVersions)
		return nil

	case "export":
		if *to == "" {
			return fmt.Errorf("ob db export: --to DIR is required")
		}
		if ents, err := os.ReadDir(*to); err == nil && len(ents) > 0 {
			return fmt.Errorf("%s is not empty; export into a new directory", *to)
		} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		n := 0
		err := store.Export(ctx, db, func(name string, b []byte) error {
			p := filepath.Join(*to, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			n++
			return os.WriteFile(p, b, 0o644)
		})
		if err != nil {
			return err
		}
		fmt.Printf("Exported %s to %s (%d files).\n", label, *to, n)
		return nil

	case "list":
		src, err := store.Open(ctx, db)
		if err != nil {
			return err
		}
		return report.ListSnapshots(stdout, src.FS, src.Snapshots)

	case "show":
		if *snap == "" {
			return fmt.Errorf("ob db show: --snapshot ID is required")
		}
		at, err := atFor(spec, *snap)
		if err != nil {
			return err
		}
		return report.GetSnapshot(stdout, at, raw)

	case "verify":
		rep, err := store.Verify(ctx, db)
		if err != nil {
			return err
		}
		fmt.Fprint(stdout, rep.Summary(label))
		return nil
	}
	usage()
	return nil
}
