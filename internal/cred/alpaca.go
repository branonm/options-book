package cred

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// AlpacaPaperEndpoint is the only Alpaca endpoint a credential may name or be
// sent to. internal/alpaca pins the same URL (a test there checks they agree).
const AlpacaPaperEndpoint = "https://paper-api.alpaca.markets"

const alpacaPaperHost = "paper-api.alpaca.markets"

// AlpacaDataHost serves market data (quotes, snapshots, bars). It has no
// trading endpoint and the same host serves paper and live accounts, so the key
// is sent to it for GET requests only, never for anything that changes state.
const AlpacaDataHost = "data.alpaca.markets"

// AlpacaPaperSecret is the alpaca-paper secret.
type AlpacaPaperSecret struct {
	KeyID      string `json:"key_id"`
	SecretKey  string `json:"secret_key"`
	Endpoint   string `json:"endpoint"`
	EnrolledAt string `json:"enrolled_at"`
}

// AlpacaPaper is the alpaca-paper provider. It never refreshes.
type AlpacaPaper struct {
	HC  *http.Client
	Now func() time.Time
}

func NewAlpacaPaper(hc *http.Client) *AlpacaPaper {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &AlpacaPaper{HC: hc, Now: time.Now}
}

func (a *AlpacaPaper) ID() string { return "alpaca-paper" }

// pinned refuses anything but the paper endpoint.
func pinned(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host != alpacaPaperHost || (u.Path != "" && u.Path != "/") {
		return fmt.Errorf("alpaca-paper: endpoint %q is not %s; only paper trading is allowed", endpoint, AlpacaPaperEndpoint)
	}
	return nil
}

func parseAlpaca(s Secret) (*AlpacaPaperSecret, error) {
	var as AlpacaPaperSecret
	if err := json.Unmarshal(s, &as); err != nil {
		return nil, fmt.Errorf("alpaca-paper: not the expected JSON")
	}
	if as.KeyID == "" || as.SecretKey == "" {
		return nil, fmt.Errorf("alpaca-paper: missing key_id or secret_key")
	}
	if err := pinned(as.Endpoint); err != nil {
		return nil, err
	}
	return &as, nil
}

func (a *AlpacaPaper) Expires(Secret) (time.Time, bool) { return time.Time{}, false }

func (a *AlpacaPaper) Refresh(context.Context, Secret) (Secret, error) { return nil, ErrNotRefreshable }

// Authorize adds the key to a request for the paper host, or a GET to the
// market-data host — checked on every use, not just at enroll.
func (a *AlpacaPaper) Authorize(_ context.Context, s Secret, req *http.Request) error {
	as, err := parseAlpaca(s)
	if err != nil {
		return err
	}
	dataRead := req.URL.Host == AlpacaDataHost && req.Method == http.MethodGet
	if req.URL.Scheme != "https" || (req.URL.Host != alpacaPaperHost && !dataRead) {
		return fmt.Errorf("alpaca-paper: the key is only sent to %s (and, for GET, %s), not %s %s", AlpacaPaperEndpoint, AlpacaDataHost, req.Method, req.URL.Redacted())
	}
	req.Header.Set("APCA-API-KEY-ID", as.KeyID)
	req.Header.Set("APCA-API-SECRET-KEY", as.SecretKey)
	return nil
}

// Enroll reads the key at a hidden prompt (never from argv) and verifies it
// with one account call against the paper endpoint.
func (a *AlpacaPaper) Enroll(ctx context.Context, ui UI) (Secret, error) {
	ui.Printf("Alpaca paper trading only (%s). Paste the paper account's API key.\n", AlpacaPaperEndpoint)
	id, err := ui.Line("Key ID: ")
	if err != nil {
		return nil, err
	}
	secret, err := ui.Hidden("Secret key (not shown): ")
	if err != nil {
		return nil, err
	}
	s, err := json.Marshal(AlpacaPaperSecret{
		KeyID: strings.TrimSpace(id), SecretKey: strings.TrimSpace(secret),
		Endpoint: AlpacaPaperEndpoint, EnrolledAt: a.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return nil, err
	}
	if _, err := parseAlpaca(s); err != nil {
		return nil, err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, AlpacaPaperEndpoint+"/v2/account", nil)
	if err := a.Authorize(ctx, s, req); err != nil {
		return nil, err
	}
	resp, err := a.HC.Do(req)
	if err != nil {
		return nil, fmt.Errorf("verifying the key: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var acct struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(b, &acct) != nil || acct.ID == "" {
		return nil, fmt.Errorf("verifying the key: the paper account call returned HTTP %d", resp.StatusCode)
	}
	ui.Printf("Verified against the paper account (status %s).\n", acct.Status)
	return s, nil
}
