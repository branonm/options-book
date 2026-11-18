package edgar

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/time/rate"
)

const (
	tickersURL        = "https://www.sec.gov/files/company_tickers.json"
	submissionsURLFmt = "https://data.sec.gov/submissions/CIK%s.json"
)

// Config holds the identification SEC's fair-access policy requires:
// https://www.sec.gov/os/accessing-edgar-data.
type Config struct {
	// UserAgent must be "Company Name contact@domain.com". There is no
	// default — the caller supplies a real contact, never one guessed or
	// filled in on their behalf.
	UserAgent string
	// RequestsPerSecond caps this client's own request rate. Zero defaults
	// to 8, under SEC's stated 10/s maximum; values over 10 are refused.
	RequestsPerSecond float64
	HTTPClient        *http.Client
}

// Client fetches SEC EDGAR's public submissions and ticker-lookup APIs. It
// reads public filings only: no broker, no order, no authentication.
type Client struct {
	userAgent string
	http      *http.Client
	limiter   *rate.Limiter

	// Overridable only by tests in this package, to point at an httptest
	// server instead of SEC's real hosts.
	tickersURL        string
	submissionsURLFmt string
}

func NewClient(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.UserAgent) == "" {
		return nil, fmt.Errorf(`edgar: UserAgent is required, "Company Name contact@domain.com" (SEC fair-access policy)`)
	}
	rps := cfg.RequestsPerSecond
	if rps <= 0 {
		rps = 8
	}
	if rps > 10 {
		return nil, fmt.Errorf("edgar: RequestsPerSecond %.1f exceeds SEC's 10/s fair-access limit", rps)
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{
		userAgent:         cfg.UserAgent,
		http:              httpClient,
		limiter:           rate.NewLimiter(rate.Limit(rps), 1),
		tickersURL:        tickersURL,
		submissionsURLFmt: submissionsURLFmt,
	}, nil
}

func (c *Client) get(ctx context.Context, url string) ([]byte, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("edgar: GET %s: %s", url, resp.Status)
	}
	return body, nil
}

// LookupCIK finds a ticker's zero-padded 10-digit CIK from SEC's
// company_tickers.json. The match is case-insensitive.
func (c *Client) LookupCIK(ctx context.Context, ticker string) (string, error) {
	body, err := c.get(ctx, c.tickersURL)
	if err != nil {
		return "", err
	}
	var raw map[string]CompanyTicker
	if err := json.Unmarshal(body, &raw); err != nil {
		return "", fmt.Errorf("edgar: decoding company_tickers.json: %w", err)
	}
	want := strings.ToUpper(strings.TrimSpace(ticker))
	for _, ct := range raw {
		if strings.ToUpper(ct.Ticker) == want {
			return fmt.Sprintf("%010d", ct.CIK), nil
		}
	}
	return "", fmt.Errorf("edgar: no CIK found for ticker %q", ticker)
}

// Submissions fetches a company's filing history by CIK (zero-padded to 10
// digits — the form LookupCIK returns).
func (c *Client) Submissions(ctx context.Context, cik string) (*Submissions, error) {
	body, err := c.get(ctx, fmt.Sprintf(c.submissionsURLFmt, cik))
	if err != nil {
		return nil, err
	}
	var raw submissionsRaw
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("edgar: decoding submissions for CIK %s: %w", cik, err)
	}
	return zipSubmissions(raw), nil
}

func zipSubmissions(raw submissionsRaw) *Submissions {
	cols := raw.Filings.Recent
	s := &Submissions{CIK: raw.CIK, Name: raw.Name, Tickers: raw.Tickers}
	for i := range cols.AccessionNumber {
		s.Filings = append(s.Filings, Filing{
			AccessionNumber: cols.AccessionNumber[i],
			FilingDate:      at(cols.FilingDate, i),
			ReportDate:      at(cols.ReportDate, i),
			Form:            at(cols.Form, i),
			Items:           at(cols.Items, i),
			PrimaryDocument: at(cols.PrimaryDocument, i),
		})
	}
	return s
}

func at(col []string, i int) string {
	if i < len(col) {
		return col[i]
	}
	return ""
}

// FilterForms returns the filings whose Form is one of forms, preserving
// order (SEC returns newest first).
func FilterForms(filings []Filing, forms ...string) []Filing {
	want := make(map[string]bool, len(forms))
	for _, f := range forms {
		want[f] = true
	}
	var out []Filing
	for _, f := range filings {
		if want[f.Form] {
			out = append(out, f)
		}
	}
	return out
}

// DocumentURL builds the public URL for a filing's primary document. cik may
// carry leading zeros (as returned by LookupCIK); accessionNumber may carry
// its dashes.
func DocumentURL(cik, accessionNumber, primaryDocument string) string {
	noDash := strings.ReplaceAll(accessionNumber, "-", "")
	trimmed := strings.TrimLeft(cik, "0")
	if trimmed == "" {
		trimmed = "0"
	}
	return fmt.Sprintf("https://www.sec.gov/Archives/edgar/data/%s/%s/%s", trimmed, noDash, primaryDocument)
}
