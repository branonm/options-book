package store

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/branonm/options-book/internal/snapshot"
)

// repoFS is the repo as the files Source sees it.
func repoFS(t *testing.T) fs.FS {
	t.Helper()
	root, err := filepath.Abs(repoRoot())
	if err != nil {
		t.Fatal(err)
	}
	return os.DirFS(root)
}

func importRepo(t *testing.T, db DB) *ImportReport {
	t.Helper()
	rep, err := ImportRepo(context.Background(), db, repoFS(t))
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// dataFiles lists every file under the store's part of the repo layout.
func dataFiles(t *testing.T, fsys fs.FS) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, top := range []string{"snapshots", "history", "reconcile"} {
		err := fs.WalkDir(fsys, top, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			out[p] = true
			return nil
		})
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
	}
	return out
}

// TestImportReproducesRepo is phase 2's first gate: importing the repo and
// reading it back reproduces every data file byte for byte — so every sha256
// in every manifest, every history.json, and both order logs (and therefore
// both logs' prefix hashes) — except the files the importer reports skipping.
func TestImportReproducesRepo(t *testing.T) {
	db := NewMem()
	rep := importRepo(t, db)
	src, err := Open(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	disk := repoFS(t)
	want := dataFiles(t, disk)
	got := dataFiles(t, src.FS)

	skipped := map[string]bool{}
	for _, s := range rep.Skipped {
		p, _, _ := strings.Cut(s, ":")
		skipped[p] = true
	}
	for p := range want {
		switch {
		case skipped[p]:
			if got[p] {
				t.Errorf("%s: reported skipped but present", p)
			}
		case strings.HasPrefix(p, "history/bars/"):
			// Stored as raw_response bodies cited by expiration_outcome.bar_sha256, not as files.
		case !got[p]:
			t.Errorf("%s: missing from the store", p)
		default:
			a, _ := fs.ReadFile(disk, p)
			b, _ := fs.ReadFile(src.FS, p)
			if !bytes.Equal(a, b) {
				t.Errorf("%s: differs from the file", p)
			}
		}
	}
	for p := range got {
		if !want[p] {
			t.Errorf("%s: in the store but not in the repo", p)
		}
	}
	t.Logf("import: %+v; compared %d data files, store presents %d", *rep, len(want), len(got))
	if rep.OrderVersions == 0 || rep.Seals == 0 || rep.OrderPos == 0 {
		t.Fatalf("import wrote too little: %+v", rep)
	}
	var ids []string
	ents, _ := fs.ReadDir(disk, "snapshots")
	for _, e := range ents {
		if e.IsDir() {
			ids = append(ids, e.Name())
		}
	}
	sort.Strings(ids)
	if strings.Join(ids, ",") != strings.Join(src.Snapshots, ",") {
		t.Errorf("snapshots: %v, want %v", src.Snapshots, ids)
	}

	// The manifests also check out the way `ob snapshot` checks them.
	for _, id := range src.Snapshots {
		at := src.At(id)
		m, err := snapshot.ReadManifestAt(at)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		cur, err := snapshot.HashInputs(at)
		if err != nil {
			t.Fatal(err)
		}
		if len(cur) != len(m.Files) {
			t.Errorf("%s: %d inputs, manifest lists %d", id, len(cur), len(m.Files))
		}
		for rel, sha := range m.Files {
			if cur[rel] != sha {
				t.Errorf("%s/%s: sha256 %s, manifest %s", id, rel, cur[rel], sha)
			}
		}
	}
}

// Re-running an import over a complete store writes nothing new and fails nothing.
func TestImportIsRerunnable(t *testing.T) {
	db := NewMem()
	importRepo(t, db)
	importRepo(t, db)
	if _, err := Open(context.Background(), db); err != nil {
		t.Fatal(err)
	}
}

// tampered imports the repo, then edits the in-memory documents directly —
// something IAM forbids in production, which is exactly why Open must notice.
func tampered(t *testing.T, edit func(m *Mem)) error {
	t.Helper()
	db := NewMem()
	importRepo(t, db)
	edit(db)
	_, err := Open(context.Background(), db)
	return err
}

func TestOpenRefusesTampering(t *testing.T) {
	ctx := context.Background()
	cases := map[string]func(m *Mem){
		"member added to a sealed snapshot": func(m *Mem) {
			sha := ""
			for id := range m.docs[CRawResponse] {
				sha = id
				break
			}
			m.Create(ctx, CMember, Member{"2026-02-13", RoleOrdersPull, "roth", "extra_p1"}.ID(),
				Doc{"snapshot_id": "2026-02-13", "role": RoleOrdersPull, "account": "roth", "instrument_id": "extra_p1", "sha256": sha})
		},
		"member repointed at another body": func(m *Mem) {
			id := Member{"2026-02-13", RoleEquityQuotes, "", ""}.ID()
			other := m.docs[CMember][Member{"2026-02-06", RoleEquityQuotes, "", ""}.ID()]["sha256"]
			m.docs[CMember][id]["sha256"] = other
		},
		"body bytes changed": func(m *Mem) {
			for coll, docs := range m.docs {
				if strings.HasSuffix(coll, "/"+CChunk) {
					b := docs["0000"]["body"].([]byte)
					b[len(b)/2] ^= 1
					return
				}
			}
		},
		"log line edited": func(m *Mem) {
			d := m.docs[COrderVersion][orderVersionID("roth", 5)]
			d["observed_at"] = "2026-11-04T17:33:01Z"
		},
		"log line removed": func(m *Mem) {
			delete(m.docs[COrderVersion], orderVersionID("trad", 2))
		},
		"order position moved": func(m *Mem) {
			d := m.docs[COrderPos]["2026-02-13|roth"]
			d["seq"] = d["seq"].(int64) - 1
		},
		"expiration outcome flipped": func(m *Mem) {
			for _, d := range m.docs[CExpiration] {
				if d["outcome"] == "assigned" {
					d["outcome"] = "expired_worthless"
				} else {
					d["outcome"] = "assigned" // the synthetic repo's only outcome is expired_worthless
				}
				return
			}
		},
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			if err := tampered(t, edit); err == nil {
				t.Fatal("Open accepted a tampered store")
			} else {
				t.Log(err)
			}
		})
	}
}

// A stored document is never changed: re-importing over a differing one fails.
func TestImportRefusesToOverwrite(t *testing.T) {
	db := NewMem()
	importRepo(t, db)
	db.docs[CSnapshot]["2026-02-13"]["note"] = "edited"
	if _, err := ImportRepo(context.Background(), db, repoFS(t)); err == nil {
		t.Fatal("import overwrote a differing document")
	}
}

// the storage design: `ob db verify` checks that export → import → export is
// byte-identical, and the export is deterministic.
func TestExportImportExport(t *testing.T) {
	ctx := context.Background()
	db := NewMem()
	importRepo(t, db)
	rep, err := Verify(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("verify: %+v", *rep)
	// Counted from the repo, not written down: the next check-in changes them (B4).
	disk := repoFS(t)
	dirs, _ := fs.Glob(disk, "snapshots/2*")
	manifests, _ := fs.Glob(disk, "snapshots/*/manifest.json")
	lines := 0
	for _, acct := range []string{"roth", "trad"} {
		b, _ := fs.ReadFile(disk, LogPath(acct))
		lines += bytes.Count(b, []byte("\n"))
	}
	if rep.Snapshots != len(dirs) || rep.Sealed != len(manifests) || rep.OrderVersions != lines {
		t.Errorf("verify counted %+v; the repo has %d snapshots, %d sealed, %d log lines", *rep, len(dirs), len(manifests), lines)
	}

	a, b := map[string][]byte{}, map[string][]byte{}
	if err := Export(ctx, db, mapSink(a)); err != nil {
		t.Fatal(err)
	}
	if err := Export(ctx, db, mapSink(b)); err != nil {
		t.Fatal(err)
	}
	if err := sameExport(a, b); err != nil {
		t.Fatalf("two exports of one store: %v", err)
	}

	// A store rebuilt from the export reads back the same repo layout.
	rebuilt := NewMem()
	if err := ImportExport(ctx, rebuilt, exportFS(a)); err != nil {
		t.Fatal(err)
	}
	s1, _ := Open(ctx, db)
	s2, err := Open(ctx, rebuilt)
	if err != nil {
		t.Fatal(err)
	}
	f1, f2 := dataFiles(t, s1.FS), dataFiles(t, s2.FS)
	if len(f1) != len(f2) {
		t.Fatalf("%d files vs %d", len(f1), len(f2))
	}
	for p := range f1 {
		x, _ := fs.ReadFile(s1.FS, p)
		y, _ := fs.ReadFile(s2.FS, p)
		if !bytes.Equal(x, y) {
			t.Errorf("%s differs after export → import", p)
		}
	}

	// A body altered in the export is refused.
	for k, v := range a {
		if strings.HasPrefix(k, "bodies/") {
			v[0] ^= 1
			break
		}
	}
	if err := ImportExport(ctx, NewMem(), exportFS(a)); err == nil {
		t.Error("import accepted an altered body")
	}
}

// WHEEL_COMPARE_EXPORT=<dir> checks a `ob db export` of some other store
// (the live one, say) against an export of the repo imported here: equal
// exports mean that store holds exactly what this test suite has verified.
func TestExportMatchesDir(t *testing.T) {
	dir := os.Getenv("WHEEL_COMPARE_EXPORT")
	if dir == "" {
		t.Skip("WHEEL_COMPARE_EXPORT not set")
	}
	db := NewMem()
	importRepo(t, db)
	want := map[string][]byte{}
	if err := Export(context.Background(), db, mapSink(want)); err != nil {
		t.Fatal(err)
	}
	got := map[string][]byte{}
	err := fs.WalkDir(os.DirFS(dir), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		got[p], err = os.ReadFile(filepath.Join(dir, p))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sameExport(want, got); err != nil {
		t.Fatal(err)
	}
	t.Logf("%s matches: %d files", dir, len(got))
}
