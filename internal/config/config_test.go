package config

import (
	"strings"
	"testing"
)

func TestParseAccountsRefuses(t *testing.T) {
	good := `{"name":"a","label":"A","last4":"1234","source":"robinhood","modes":["manual"]}`
	for name, c := range map[string]struct{ json, want string }{
		"full account number": {`{"accounts":[{"name":"a","label":"A","last4":"123456789","source":"robinhood","modes":["manual"]}]}`, "exactly four letters or digits"},
		"no source":           {`{"accounts":[{"name":"a","label":"A","last4":"1234","modes":["manual"]}]}`, "no source"},
		"no modes":            {`{"accounts":[{"name":"a","label":"A","last4":"1234","source":"robinhood","modes":[]}]}`, "permits no execution mode"},
		"unknown mode":        {`{"accounts":[{"name":"a","label":"A","last4":"1234","source":"robinhood","modes":["telepathy"]}]}`, "unknown execution mode"},
		"duplicate":           {`{"accounts":[` + good + `,` + good + `]}`, "listed twice"},
		"unknown field":       {`{"accounts":[{"name":"a","label":"A","last4":"1234","source":"robinhood","modes":["manual"],"balance":"1"}]}`, "unknown field"},
		"no name":             {`{"accounts":[{"label":"A","last4":"1234","source":"robinhood","modes":["manual"]}]}`, "name and a label"},
	} {
		if _, err := ParseAccounts("accounts.json", []byte(c.json)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestParseBookRefuses(t *testing.T) {
	accts, err := ParseAccounts("accounts.json", []byte(`{"accounts":[
		{"name":"roth","label":"Roth","last4":"0001","source":"robinhood","default_book":"ira-wheel","modes":["manual"]},
		{"name":"tax","label":"Tax","last4":"0003","source":"robinhood","default_book":null,"modes":["manual"]},
		{"name":"other","label":"Other","last4":"1111","source":"robinhood","default_book":"paper","modes":["manual"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	book := func(j string) error {
		_, err := ParseBook("ira-wheel", "book.json", []byte(j), accts)
		return err
	}
	if err := book(`{"strategy":"wheel","campaign_start":"2026-08-19","accounts":[{"name":"roth","pool":"p"}]}`); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct{ json, want string }{
		"no campaign start":    {`{"strategy":"wheel","accounts":[]}`, "campaign_start is required"},
		"unknown strategy":     {`{"strategy":"condor","campaign_start":"2026-08-19","accounts":[]}`, `strategy "condor"`},
		"no strategy":          {`{"campaign_start":"2026-08-19","accounts":[]}`, "strategy"},
		"account not in file":  {`{"strategy":"wheel","campaign_start":"2026-08-19","accounts":[{"name":"ghost"}]}`, "is not in config/accounts.json"},
		"account with no book": {`{"strategy":"wheel","campaign_start":"2026-08-19","accounts":[{"name":"tax"}]}`, `default book is "", not "ira-wheel"`},
		"another book's":       {`{"strategy":"wheel","campaign_start":"2026-08-19","accounts":[{"name":"other"}]}`, `default book is "paper"`},
		"listed twice":         {`{"strategy":"wheel","campaign_start":"2026-08-19","accounts":[{"name":"roth"},{"name":"roth"}]}`, "listed twice"},
		"label in the book":    {`{"strategy":"wheel","campaign_start":"2026-08-19","accounts":[{"name":"roth","label":"Roth"}]}`, "unknown field"},
		"typo in a field":      {`{"strategy":"wheel","campaign_start":"2026-08-19","acounts":[]}`, "unknown field"},
	} {
		if err := book(c.json); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Alpaca account numbers contain letters, so last4 is four letters or digits.
func TestParseAccountsAcceptsLetteredLast4(t *testing.T) {
	a, err := ParseAccounts("accounts.json", []byte(`{"accounts":[{"name":"p","label":"P","last4":"AB12","source":"alpaca","modes":["manual"]}]}`))
	if err != nil || len(a.Accounts) != 1 || a.Accounts[0].Last4 != "AB12" {
		t.Fatalf("%v %+v", err, a)
	}
}
