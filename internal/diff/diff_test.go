package diff

import (
	"io/fs"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/branonm/options-book/internal/config"
	"github.com/branonm/options-book/internal/snapshot"
	"github.com/branonm/options-book/internal/strategy/wheel"
)

func side(t *testing.T, fsys fs.FS, cfg *config.Book, name string) Side {
	t.Helper()
	asOf, _ := time.Parse("2006-01-02", name)
	var names []string
	for _, a := range cfg.Accounts {
		names = append(names, a.Name)
	}
	s, err := snapshot.LoadAt(snapshot.At{FS: fsys, Dir: "snapshots/" + name}, names)
	if err != nil {
		t.Fatal(err)
	}
	bk, err := wheel.Assemble(cfg, s, asOf)
	if err != nil {
		t.Fatal(err)
	}
	return Side{Label: name, Book: bk, Snap: s}
}

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }
