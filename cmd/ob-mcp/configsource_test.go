package main

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/branonm/options-book/internal/layout"
)

type fakeObjects map[string]string

func (f fakeObjects) List(_ context.Context, prefix string) ([]string, error) {
	var out []string
	for n := range f {
		if strings.HasPrefix(n, prefix) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (f fakeObjects) Read(_ context.Context, name string) ([]byte, int64, error) {
	v, ok := f[name]
	if !ok {
		return nil, 0, errors.New("not found")
	}
	return []byte(v), 7, nil
}

func TestBucketConfigLoadsAndRecordsGenerations(t *testing.T) {
	emb, err := embeddedConfig()
	if err != nil {
		t.Fatal(err)
	}
	f := fakeObjects{layout.AccountsConfig: string(emb.Accounts)}
	for n, b := range emb.Books {
		f[layout.BookFile(n)] = string(b.Book)
		f[layout.RulesFile(n)] = string(b.Rules)
	}
	cs, err := bucketConfig(context.Background(), f, "b")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs.Books) != len(emb.Books) || !strings.Contains(cs.Origin, "accounts.json#7") || !strings.HasPrefix(cs.Origin, "bucket b") {
		t.Fatalf("got %d books, origin %q", len(cs.Books), cs.Origin)
	}
}

// Fail closed: a missing object is an error, never the compiled-in defaults.
func TestBucketConfigFailsClosed(t *testing.T) {
	ctx := context.Background()
	if _, err := bucketConfig(ctx, fakeObjects{}, "b"); err == nil {
		t.Fatal("an empty bucket must be an error")
	}
	if _, err := bucketConfig(ctx, fakeObjects{layout.AccountsConfig: "{}"}, "b"); err == nil || !strings.Contains(err.Error(), "no books") {
		t.Fatalf("no books must be an error, got %v", err)
	}
	half := fakeObjects{layout.AccountsConfig: "{}", layout.BookFile("x"): "{}"}
	if _, err := bucketConfig(ctx, half, "b"); err == nil || !strings.Contains(err.Error(), "rules.json") {
		t.Fatalf("a book without rules.json must be an error, got %v", err)
	}
}
