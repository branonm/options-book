package alpacapull

import (
	"context"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rewrite sends every request to the test server, whatever host it names, so
// the allowlist and credential checks run against the real hosts.
type rewrite struct{ to *url.URL }

func (r rewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	c := req.Clone(req.Context())
	c.Header.Set("X-Real-Host", req.URL.Host)
	c.URL.Scheme, c.URL.Host = r.to.Scheme, r.to.Host
	return http.DefaultTransport.RoundTrip(c)
}

func TestReadsAreGetAndAllowlisted(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.Header.Get("X-Real-Host")+r.URL.RequestURI())
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c := &Client{HC: &http.Client{Transport: rewrite{u}}, Authorize: func(_ context.Context, r *http.Request) error { r.Header.Set("APCA-API-KEY-ID", "k"); return nil }}
	ctx := context.Background()
	for _, f := range []func() (*Response, error){
		func() (*Response, error) { return c.Account(ctx) },
		func() (*Response, error) { return c.Positions(ctx) },
		func() (*Response, error) { return c.Orders(ctx, "all", "2026-11-01T00:00:00Z") },
		func() (*Response, error) { return c.Order(ctx, "6c1d0a10-0000-4000-8000-000000000001") },
		func() (*Response, error) { return c.LatestTrades(ctx, []string{"AAA"}) },
		func() (*Response, error) { return c.OptionSnapshots(ctx, []string{"AAA270617C00055000"}) },
		func() (*Response, error) { return c.OptionContracts(ctx, []string{"AAA270617C00055000"}) },
	} {
		if _, err := f(); err != nil {
			t.Error(err)
		}
	}
	if len(seen) != 7 {
		t.Fatalf("seen %v", seen)
	}
	for _, s := range seen {
		if !strings.HasPrefix(s, "GET ") {
			t.Errorf("a non-GET request: %s", s)
		}
	}
	if !strings.Contains(seen[2], "status=all") || !strings.Contains(seen[2], "limit=500") || !strings.Contains(seen[2], "after=2026-11-01") {
		t.Errorf("orders query: %s", seen[2])
	}
	// Paths off the allowlist never leave the process.
	n := len(seen)
	for _, p := range [][2]string{
		{PaperHost, "/v2/orders/../account/configurations"}, {PaperHost, "/v2/account/configurations"}, {PaperHost, "/v2/positions/AAA"},
		{PaperHost, "/v2/options/exercise"}, {PaperHost, "/v2/options/contracts/AAA270617C00055000"}, {DataHost, "/v2/options/contracts"}, {"api.alpaca.markets", "/v2/account"}, {DataHost, "/v2/orders"}, {PaperHost, "/v2/stocks/trades/latest"},
	} {
		if _, err := c.Get(ctx, p[0], p[1], nil); err == nil {
			t.Errorf("%s%s was allowed", p[0], p[1])
		}
	}
	if len(seen) != n {
		t.Error("a refused path was requested")
	}
}

func TestErrorsAreReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		w.Write([]byte(`{"message":"forbidden"}`))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c := &Client{HC: &http.Client{Transport: rewrite{u}}, Authorize: func(context.Context, *http.Request) error { return nil }}
	if _, err := c.Account(context.Background()); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("a 403 must be an error: %v", err)
	}
	if _, err := (&Client{}).Account(context.Background()); err == nil {
		t.Error("a client with no credential must refuse")
	}
}

// Nothing in this package can place, replace or cancel an order: it never
// names a method other than GET, and imports no Alpaca trading SDK.
func TestNoWriteMethodsAnywhere(t *testing.T) {
	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return err
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, 0)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			if strings.Contains(imp.Path.Value, "alpacahq") || strings.Contains(imp.Path.Value, "internal/alpaca\"") {
				t.Errorf("%s imports %s", p, imp.Path.Value)
			}
		}
		src, _ := os.ReadFile(p)
		for _, bad := range []string{"MethodPost", "MethodPut", "MethodPatch", "MethodDelete", `"POST"`, `"PUT"`, `"PATCH"`, `"DELETE"`} {
			if strings.Contains(string(src), bad) {
				t.Errorf("%s names %s", p, bad)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
