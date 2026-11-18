package broker_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestOnlyTheAdapterImportsRH is item 6 step 1's pass condition, kept as a
// build-failing test: nothing outside internal/rh itself and the Robinhood
// adapter imports Robinhood's response shapes. Everything else works on
// internal/broker's canonical types, so a second broker needs an adapter and
// nothing else.
func TestOnlyTheAdapterImportsRH(t *testing.T) {
	root := filepath.Join("..", "..")
	const rhPath = `"github.com/branonm/options-book/internal/rh"`
	allowed := func(dir string) bool {
		d := filepath.ToSlash(dir)
		return d == "internal/rh" || d == "internal/broker/robinhood"
	}
	checked := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".idea", "bin", "snapshots", "history", "reconcile", "infra":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		for _, imp := range f.Imports {
			if imp.Path.Value == rhPath && !allowed(filepath.Dir(rel)) {
				t.Errorf("%s imports internal/rh; decode through internal/broker (the adapter is the only importer)", rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 50 {
		t.Fatalf("only %d Go files checked; the walk is broken", checked)
	}
}
