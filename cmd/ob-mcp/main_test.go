package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/branonm/options-book/internal/gauth"
	"github.com/branonm/options-book/internal/layout"
	"github.com/branonm/options-book/internal/store"
	"github.com/branonm/options-book/internal/store/storetest"
)

type fakeVerifier struct{}

func (fakeVerifier) Verify(_ context.Context, tok string) (string, error) {
	if tok == "good" {
		return "owner@example.com", nil
	}
	return "", errors.New("refused")
}

type bearer struct{ tok string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.tok)
	return http.DefaultTransport.RoundTrip(r)
}

// The deployed stack end to end: the compiled-in config, the token check, and
// the tools over stateless streamable HTTP, against the repo in a store.
func TestServerOverHTTP(t *testing.T) {
	ctx := context.Background()
	// The emulator path needs no credentials; its store is then swapped for the
	// repo imported into memory, keeping the parsed compiled-in config.
	deps, err := newDeps("test-project", "(default)", "localhost:1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	db := store.NewMem()
	if _, err := store.ImportRepo(ctx, db, os.DirFS(storetest.Root(t))); err != nil {
		t.Fatal(err)
	}
	deps.DB, deps.Spec = db, "memory"

	cfg := gauth.Config{ClientID: "c", AllowedEmail: "owner@example.com", ResourceName: "options-book", LocalTesting: true}
	srv := httptest.NewServer(nil)
	defer srv.Close()
	cfg.PublicURL = srv.URL
	srv.Config.Handler = newHandler(cfg, fakeVerifier{}, deps)

	// Without a token: 401 pointing at the metadata.
	resp, err := http.Post(srv.URL+"/mcp", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(resp.Header.Get("WWW-Authenticate"), "resource_metadata=") {
		t.Fatalf("no token: %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}

	// With one: a full MCP session.
	tr := &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp", HTTPClient: &http.Client{Transport: bearer{"good"}}, MaxRetries: -1, DisableStandaloneSSE: true}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, tr, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if got := cs.InitializeResult().Instructions; !strings.Contains(got, "places no orders") {
		t.Errorf("instructions: %q", got)
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "checkin", Arguments: map[string]any{"snapshot": "2026-02-13"}})
	if err != nil {
		t.Fatal(err)
	}
	txt := res.Content[0].(*mcp.TextContent).Text
	if res.IsError || !strings.HasPrefix(txt, "# Check-in as of 2026-02-13\n") {
		t.Errorf("checkin over HTTP: error=%v %q", res.IsError, txt[:min(len(txt), 120)])
	}
	runs, _ := db.List(ctx, store.CRun)
	if len(runs) != 1 || runs[0].Doc["tool_commit"] != deps.Commit || deps.Commit == "" {
		t.Errorf("run documents: %+v (commit %q)", runs, deps.Commit)
	}
}

// The compiled-in config is the repo's config, byte for byte.
func TestCompiledInConfig(t *testing.T) {
	deps, err := newDeps("p", "(default)", "localhost:1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{layout.AccountsConfig: deps.AccountsSHA}
	for name, bc := range deps.Books {
		want[layout.BookFile(name)] = bc.BookSHA
		want[layout.RulesFile(name)] = bc.RulesSHA
	}
	for rel, sha := range want {
		b, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		if store.SHA256(b) != sha {
			t.Errorf("%s compiled in differs from the file", rel)
		}
	}
	// Every book in the repo is compiled in, and nothing else is.
	ents, _ := os.ReadDir(filepath.Join("..", "..", layout.BooksDir))
	dirs := 0
	for _, d := range ents {
		if !d.IsDir() {
			continue
		}
		dirs++
		if deps.Books[d.Name()] == nil {
			t.Errorf("book %s is in %s/ but not compiled in", d.Name(), layout.BooksDir)
		}
	}
	if len(deps.Books) != dirs || deps.DefaultBook != layout.DefaultBook || deps.Books[deps.DefaultBook] == nil {
		t.Errorf("%d books compiled in for %d on disk; default %q", len(deps.Books), dirs, deps.DefaultBook)
	}
}
