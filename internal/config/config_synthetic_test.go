package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadIraWheel(t *testing.T) {
	b, err := Load(root, "ira-wheel")
	if err != nil {
		t.Fatal(err)
	}
	if b.Name != "ira-wheel" || b.Strategy != "wheel" || b.Doc != "wheel-book.md" || b.CampaignStart != "2026-01-05" {
		t.Errorf("book: %+v", b)
	}
	if len(b.Accounts) != 2 {
		t.Fatalf("%d accounts; the taxable account belongs to no book", len(b.Accounts))
	}
	roth, err := b.Account("roth")
	if err != nil {
		t.Fatal(err)
	}
	// The account's own record and the book's claim on it, joined.
	if roth.Label != "Roth IRA" || roth.Last4 != "0001" || roth.Source != "robinhood" || roth.DefaultBook != "ira-wheel" ||
		strings.Join(roth.Modes, ",") != "browser,manual" || roth.Pool != "roth" || strings.Join(roth.ExpectedSymbols, ",") != "AAA,BBB" {
		t.Errorf("roth: %+v", roth)
	}
	if _, err := b.Account("taxable"); err == nil {
		t.Error("a taxable account is in the ira-wheel book")
	}
}

func TestAccountsFile(t *testing.T) {
	a, err := LoadAccounts(filepath.Join(root, "config", "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct{ last4, modes, book, source string }{
		"roth":    {"0001", "browser,manual", "ira-wheel", "robinhood"},
		"trad":    {"0002", "browser,manual", "ira-wheel", "robinhood"},
		"taxable": {"0003", "browser,manual", "", "robinhood"},
	}
	if len(a.Accounts) != len(want) {
		t.Fatalf("%d accounts", len(a.Accounts))
	}
	for _, x := range a.Accounts {
		w := want[x.Name]
		if x.Last4 != w.last4 || strings.Join(x.Modes, ",") != w.modes || x.DefaultBook != w.book || x.Source != w.source {
			t.Errorf("%s: %+v, want %+v", x.Name, x, w)
		}
	}
}
