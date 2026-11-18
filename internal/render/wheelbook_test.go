package render

import (
	"flag"
	"github.com/branonm/options-book/internal/layout"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/branonm/options-book/internal/config"
	"github.com/branonm/options-book/internal/rules"
	"github.com/branonm/options-book/internal/rules/rulestest"
	"github.com/branonm/options-book/internal/snapshot"
	"github.com/branonm/options-book/internal/store/storetest"
	"github.com/branonm/options-book/internal/strategy/wheel"
)

var update = flag.Bool("update", false, "rewrite golden wheel-book.generated.md files")

// TestGoldenWheelBooks renders every snapshot that has a
// wheel-book.generated.md and requires identical output, so any change to a
// generated figure shows up as a reviewable diff. Run with -update to accept.
func TestGoldenWheelBooks(t *testing.T) {
	root := storetest.Root(t)
	cfg, err := config.Load(root, layout.DefaultBook)
	if err != nil {
		t.Fatal(err)
	}
	r, err := rules.Load(filepath.Join(root, filepath.FromSlash(layout.RulesFile(layout.DefaultBook))))
	if err != nil {
		t.Fatal(err)
	}
	rulestest.ForSnapshots(r)
	goldens, _ := filepath.Glob(filepath.Join(root, "snapshots", "*", "wheel-book.generated.md"))
	if len(goldens) == 0 {
		t.Fatal("no golden wheel-book.generated.md found")
	}
	// The goldens are files; the book is rendered from every source and must
	// match them byte for byte. -update writes from the files source only.
	for _, src := range storetest.Sources(t) {
		if *update && src.Name != "files" {
			continue
		}
		t.Run(src.Name, func(t *testing.T) { goldenWheelBooks(t, src.FS, goldens, cfg, r) })
	}
}

func goldenWheelBooks(t *testing.T, fsys fs.FS, goldens []string, cfg *config.Book, r *rules.Rules) {
	for _, g := range goldens {
		dir := filepath.Dir(g)
		t.Run(filepath.Base(dir), func(t *testing.T) {
			asOf, _ := time.Parse("2006-01-02", filepath.Base(dir))
			var names []string
			for _, a := range cfg.Accounts {
				names = append(names, a.Name)
			}
			snap, err := snapshot.LoadAt(snapshot.At{FS: fsys, Dir: "snapshots/" + filepath.Base(dir)}, names)
			if err != nil {
				t.Fatal(err)
			}
			bk, err := wheel.Assemble(cfg, snap, asOf)
			if err != nil {
				t.Fatal(err)
			}
			var b strings.Builder
			WheelBook(&b, bk, r, bk.Check(r), "snapshots/"+filepath.Base(dir))
			if *update {
				os.WriteFile(g, []byte(b.String()), 0o644)
				return
			}
			want, _ := os.ReadFile(g)
			if b.String() != string(want) {
				t.Errorf("generated wheel book differs from %s; review and rerun with -update to accept", g)
			}
		})
	}
}

func TestSplice(t *testing.T) {
	doc := "# Wheel book\n\nProse above.\n\n" + Begin + "\nold tables\n" + End + "\n\nProse below.\n"
	out, err := Splice(doc, Begin+"\nnew tables\n"+End+"\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Prose above.") || !strings.Contains(out, "Prose below.") || !strings.Contains(out, "new tables") || strings.Contains(out, "old tables") {
		t.Errorf("splice result:\n%s", out)
	}
	if _, err := Splice("no markers here", "x"); err == nil {
		t.Error("splice must refuse a doc without markers")
	}
	if _, err := Splice(doc+doc, "x"); err == nil {
		t.Error("splice must refuse a doc with two generated sections")
	}
}
