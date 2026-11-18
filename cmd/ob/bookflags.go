package main

// The flags that choose a book's configuration (PLAN.md item 6 step 3b),
// shared by every command that reads one.

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/branonm/options-book/internal/config"
	"github.com/branonm/options-book/internal/layout"
	"github.com/branonm/options-book/internal/rules"
)

type bookFlags struct {
	book     string // which book: books/<book>/{book,rules}.json
	cfg      string // --config: a book.json overriding --book's (fixtures, tests)
	accounts string // --accounts: an accounts.json overriding config/accounts.json
	rules    string // --rules: a rules.json overriding --book's
}

func (b *bookFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&b.book, "book", layout.DefaultBook, "the book to run: books/<book>/book.json and rules.json")
	fs.StringVar(&b.cfg, "config", "", "a book.json to use instead of the book's own")
	fs.StringVar(&b.accounts, "accounts", "", "an accounts.json to use instead of "+layout.AccountsConfig)
	fs.StringVar(&b.rules, "rules", "", "a rules.json to use instead of the book's own")
}

func (b bookFlags) path(override, def string) string {
	if override != "" {
		return override
	}
	return filepath.FromSlash(def)
}

// loadConfig reads the book, joined with its accounts.
func (b bookFlags) loadConfig() (*config.Book, error) {
	cfg, err := config.LoadFiles(b.book, b.path(b.cfg, layout.BookFile(b.book)), b.path(b.accounts, layout.AccountsConfig))
	return cfg, b.noBook(err, b.cfg)
}

// loadRules reads the book's rules.
func (b bookFlags) loadRules() (*rules.Rules, error) {
	r, err := rules.Load(b.path(b.rules, layout.RulesFile(b.book)))
	return r, b.noBook(err, b.rules)
}

// noBook says "no such book" when a book's own file is missing, instead of a bare
// "no such file": the person typed a book name, so name the book.
func (b bookFlags) noBook(err error, override string) error {
	if err != nil && override == "" && errors.Is(err, fs.ErrNotExist) {
		if _, derr := os.Stat(filepath.FromSlash(layout.BookDir(b.book))); derr != nil {
			return fmt.Errorf("no book %q: %s does not exist", b.book, layout.BookDir(b.book))
		}
	}
	return err
}
