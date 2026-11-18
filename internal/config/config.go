// Package config loads a book's configuration (PLAN.md item 6 step 3b):
//
//	config/accounts.json         every account: identity, source, default book, execution modes
//	books/<book>/book.json       a book: its strategy, campaign start, and the accounts it covers
//
// Load joins the two into a Book whose Accounts carry both the account's own
// fields and the book's claim on it (pool, expected symbols, block starts), so
// code downstream reads one shape. Numeric trading rules are separate:
// books/<book>/rules.json (internal/rules).
//
// Accounts hold the data and books make claims on it. Until claims exist
// (item 6 step 6) a book may only cover accounts it is the default book of.
package config

import (
	"encoding/json"
	"fmt"
	"github.com/branonm/options-book/internal/strategy"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/branonm/options-book/internal/layout"
)

// Execution modes an account may permit (PLAN.md addendum B).
const (
	ModeAgentic = "agentic" // Claude places via the broker's MCP connector after a read-back and the owner's yes
	ModeBrowser = "browser" // Claude fills the broker's own web order form, reads it back, submits after the owner's yes
	ModeManual  = "manual"  // the owner enters it
)

var knownModes = []string{ModeAgentic, ModeBrowser, ModeManual}

// Account is one account as a book sees it: the account's own record
// (accounts.json) and the book's claim on it (book.json).
type Account struct {
	Name        string   `json:"name"`
	Label       string   `json:"label"`
	Last4       string   `json:"last4"`
	Source      string   `json:"source"`       // the broker adapter's name
	DefaultBook string   `json:"default_book"` // "" when no book owns it yet
	Modes       []string `json:"modes"`        // execution modes this account permits

	Pool            string            `json:"pool"`
	ExpectedSymbols []string          `json:"expected_symbols"`
	BlockStarts     map[string]string `json:"block_starts,omitempty"`
}

// Book is one book's configuration with its accounts joined in.
type Book struct {
	Name          string    `json:"name"`
	Strategy      string    `json:"strategy"`
	Doc           string    `json:"doc,omitempty"` // the book's live document, from the repo root
	CampaignStart string    `json:"campaign_start"`
	Accounts      []Account `json:"accounts"`
}

// Accounts is accounts.json.
type Accounts struct {
	Doc      string    `json:"_doc"`
	Accounts []Account `json:"accounts"`
}

type bookFile struct {
	Strategy      string `json:"strategy"`
	Doc           string `json:"doc"`
	CampaignStart string `json:"campaign_start"`
	Accounts      []struct {
		Name            string            `json:"name"`
		Pool            string            `json:"pool"`
		ExpectedSymbols []string          `json:"expected_symbols"`
		BlockStarts     map[string]string `json:"block_starts"`
	} `json:"accounts"`
}

var last4 = regexp.MustCompile(`^[0-9A-Za-z]{4}$`)

// ParseAccounts decodes and checks accounts.json bytes; name labels errors.
func ParseAccounts(name string, b []byte) (*Accounts, error) {
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	var f Accounts
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	seen := map[string]bool{}
	for _, a := range f.Accounts {
		switch {
		case a.Name == "" || a.Label == "":
			return nil, fmt.Errorf("%s: every account needs a name and a label", name)
		case seen[a.Name]:
			return nil, fmt.Errorf("%s: account %q listed twice", name, a.Name)
		case !last4.MatchString(a.Last4):
			return nil, fmt.Errorf("%s: account %q: last4 must be exactly four letters or digits (never a full account number)", name, a.Name)
		case a.Source == "":
			return nil, fmt.Errorf("%s: account %q has no source (the broker adapter it is read through)", name, a.Name)
		case len(a.Modes) == 0:
			return nil, fmt.Errorf("%s: account %q permits no execution mode (%s)", name, a.Name, strings.Join(knownModes, ", "))
		}
		for _, m := range a.Modes {
			if !slices.Contains(knownModes, m) {
				return nil, fmt.Errorf("%s: account %q: unknown execution mode %q (%s)", name, a.Name, m, strings.Join(knownModes, ", "))
			}
		}
		seen[a.Name] = true
	}
	return &f, nil
}

// Account finds an account in accounts.json.
func (a *Accounts) Account(name string) (Account, bool) {
	for _, x := range a.Accounts {
		if x.Name == name {
			return x, true
		}
	}
	return Account{}, false
}

// ParseBook decodes book.json and joins it with accounts.json. book is the
// book's name; bookName and accountsName label errors (file paths).
func ParseBook(book, bookName string, bookJSON []byte, accounts *Accounts) (*Book, error) {
	dec := json.NewDecoder(strings.NewReader(string(bookJSON)))
	dec.DisallowUnknownFields()
	var f bookFile
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("%s: %w", bookName, err)
	}
	if f.CampaignStart == "" {
		return nil, fmt.Errorf("%s: campaign_start is required", bookName)
	}
	if !slices.Contains(strategy.Known(), f.Strategy) {
		return nil, fmt.Errorf("%s: strategy %q is not one of %s", bookName, f.Strategy, strings.Join(strategy.Known(), ", "))
	}
	out := &Book{Name: book, Strategy: f.Strategy, Doc: f.Doc, CampaignStart: f.CampaignStart}
	seen := map[string]bool{}
	for _, c := range f.Accounts {
		a, ok := accounts.Account(c.Name)
		switch {
		case !ok:
			return nil, fmt.Errorf("%s: account %q is not in %s", bookName, c.Name, layout.AccountsConfig)
		case seen[c.Name]:
			return nil, fmt.Errorf("%s: account %q listed twice", bookName, c.Name)
		case a.DefaultBook != book:
			// Claims (item 6 step 6) will let a book cover part of an account it
			// doesn't own; until then a book covers only accounts it is the default for.
			return nil, fmt.Errorf("%s: account %q's default book is %q, not %q", bookName, c.Name, a.DefaultBook, book)
		}
		seen[c.Name] = true
		if len(out.Accounts) > 0 && a.Source != out.Accounts[0].Source {
			return nil, fmt.Errorf("%s: a book reads one source, but %q is %s and %q is %s", bookName, out.Accounts[0].Name, out.Accounts[0].Source, c.Name, a.Source)
		}
		a.Pool, a.ExpectedSymbols, a.BlockStarts = c.Pool, c.ExpectedSymbols, c.BlockStarts
		out.Accounts = append(out.Accounts, a)
	}
	return out, nil
}

// LoadFiles reads a book from explicit files (the CLI's --config and
// --accounts overrides use this).
func LoadFiles(book, bookPath, accountsPath string) (*Book, error) {
	accounts, err := LoadAccounts(accountsPath)
	if err != nil {
		return nil, err
	}
	bb, err := os.ReadFile(bookPath)
	if err != nil {
		return nil, err
	}
	return ParseBook(book, bookPath, bb, accounts)
}

// Load reads a book from a repo root: config/accounts.json and
// books/<book>/book.json.
func Load(root, book string) (*Book, error) {
	return LoadFiles(book, filepath.Join(root, filepath.FromSlash(layout.BookFile(book))), filepath.Join(root, filepath.FromSlash(layout.AccountsConfig)))
}

// Source is the broker adapter the book's accounts are read through (a book
// reads one source; ParseBook refuses a mix).
func (c *Book) Source() string {
	if len(c.Accounts) == 0 {
		return ""
	}
	return c.Accounts[0].Source
}

// Account finds one of the book's accounts.
func (c *Book) Account(name string) (Account, error) {
	for _, a := range c.Accounts {
		if a.Name == name {
			return a, nil
		}
	}
	return Account{}, fmt.Errorf("no account %q in the %s book", name, c.Name)
}

// LoadAccounts reads accounts.json.
func LoadAccounts(path string) (*Accounts, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseAccounts(path, b)
}
