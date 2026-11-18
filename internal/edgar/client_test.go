package edgar

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := NewClient(Config{UserAgent: "options-book-test test@example.com", RequestsPerSecond: 10})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	c.tickersURL = srv.URL + "/tickers.json"
	c.submissionsURLFmt = srv.URL + "/submissions/CIK%s.json"
	return c
}

func TestNewClientRequiresUserAgent(t *testing.T) {
	if _, err := NewClient(Config{}); err == nil {
		t.Fatal("expected an error for an empty UserAgent")
	}
}

func TestNewClientRejectsOverFairAccessLimit(t *testing.T) {
	if _, err := NewClient(Config{UserAgent: "x y@z.com", RequestsPerSecond: 11}); err == nil {
		t.Fatal("expected an error for RequestsPerSecond over SEC's 10/s limit")
	}
}

func TestLookupCIK(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("User-Agent"); got != "options-book-test test@example.com" {
			t.Errorf("User-Agent = %q", got)
		}
		w.Write([]byte(`{
			"0": {"cik_str": 320193, "ticker": "AAPL", "title": "Apple Inc."},
			"1": {"cik_str": 1318605, "ticker": "TSLA", "title": "Tesla, Inc."}
		}`))
	})

	cik, err := c.LookupCIK(context.Background(), "aapl")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cik != "0000320193" {
		t.Fatalf("got CIK %q", cik)
	}

	if _, err := c.LookupCIK(context.Background(), "nope"); err == nil {
		t.Fatal("expected an error for an unknown ticker")
	}
}

func TestSubmissions(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{
			"cik": "0000320193",
			"name": "Apple Inc.",
			"tickers": ["AAPL"],
			"filings": {
				"recent": {
					"accessionNumber": ["0001140361-26-043484", "0001140361-26-042798", "0001140361-26-042505"],
					"filingDate":      ["2026-11-12", "2026-11-09", "2026-11-09"],
					"reportDate":      ["", "", "2026-09-26"],
					"form":            ["4", "8-K", "10-K"],
					"items":           ["", "2.02,9.01", ""],
					"primaryDocument": ["xslF345X06/form4.xml", "ef20085077_8k.htm", "aapl-20260926.htm"]
				}
			}
		}`))
	})

	subs, err := c.Submissions(context.Background(), "0000320193")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if subs.Name != "Apple Inc." || len(subs.Filings) != 3 {
		t.Fatalf("got %+v", subs)
	}

	tenKs := FilterForms(subs.Filings, "10-K")
	if len(tenKs) != 1 || tenKs[0].PrimaryDocument != "aapl-20260926.htm" {
		t.Fatalf("got 10-Ks %+v", tenKs)
	}

	eightKs := FilterForms(subs.Filings, "8-K")
	if len(eightKs) != 1 || eightKs[0].Items != "2.02,9.01" {
		t.Fatalf("got 8-Ks %+v", eightKs)
	}
}

func TestDocumentURL(t *testing.T) {
	got := DocumentURL("0000320193", "0001140361-26-042798", "ef20085077_8k.htm")
	want := "https://www.sec.gov/Archives/edgar/data/320193/000114036126042798/ef20085077_8k.htm"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestClientSurfacesHTTPErrors(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	})
	if _, err := c.LookupCIK(context.Background(), "aapl"); err == nil {
		t.Fatal("expected an error on a non-200 response")
	}
}
