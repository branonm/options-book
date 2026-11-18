package cred

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// Robinhood's agent OAuth, as proven by spike/rhprobe on 11/17 (PLAN.md).
const (
	RobinhoodAuthorize = "https://robinhood.com/oauth"
	RobinhoodToken     = "https://api.robinhood.com/oauth2/token/"
	RobinhoodRegister  = "https://agent.robinhood.com/oauth/trading/register"
	RobinhoodMCP       = "https://agent.robinhood.com/mcp/trading"
	RobinhoodScope     = "internal" // the only scope; there is no read-only one
)

// RobinhoodSecret is the robinhood-oauth secret.
type RobinhoodSecret struct {
	ClientID        string `json:"client_id"`
	RefreshToken    string `json:"refresh_token"`
	AccessToken     string `json:"access_token"`
	AccessExpiresAt string `json:"access_expires_at"` // RFC3339
	TokenEndpoint   string `json:"token_endpoint"`
	Resource        string `json:"resource"`
	Scope           string `json:"scope"`
	EnrolledAt      string `json:"enrolled_at"` // RFC3339
}

// Robinhood is the robinhood provider.
type Robinhood struct {
	HC                                  *http.Client
	Now                                 func() time.Time
	AuthorizeURL, TokenURL, RegisterURL string // endpoints; the constants above unless a test replaces them
	Resource                            string
	SignInTimeout                       time.Duration
	listen                              func() (net.Listener, error)
}

func NewRobinhood(hc *http.Client, now func() time.Time) *Robinhood {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	if now == nil {
		now = time.Now
	}
	return &Robinhood{
		HC: hc, Now: now,
		AuthorizeURL: RobinhoodAuthorize, TokenURL: RobinhoodToken, RegisterURL: RobinhoodRegister,
		Resource: RobinhoodMCP, SignInTimeout: 5 * time.Minute,
		listen: func() (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") },
	}
}

func (r *Robinhood) ID() string { return "robinhood" }

func parseRobinhood(s Secret) (*RobinhoodSecret, error) {
	var rs RobinhoodSecret
	if err := json.Unmarshal(s, &rs); err != nil {
		return nil, fmt.Errorf("robinhood-oauth: not the expected JSON")
	}
	if rs.AccessToken == "" || rs.RefreshToken == "" || rs.ClientID == "" || rs.TokenEndpoint == "" {
		return nil, fmt.Errorf("robinhood-oauth: missing client_id, access_token, refresh_token or token_endpoint")
	}
	return &rs, nil
}

func (r *Robinhood) Expires(s Secret) (time.Time, bool) {
	rs, err := parseRobinhood(s)
	if err != nil {
		return time.Time{}, true // unreadable counts as expired
	}
	t, err := time.Parse(time.RFC3339, rs.AccessExpiresAt)
	if err != nil {
		return time.Time{}, true
	}
	return t, true
}

// Authorize sets the bearer token on a request to Robinhood's MCP server only.
func (r *Robinhood) Authorize(_ context.Context, s Secret, req *http.Request) error {
	rs, err := parseRobinhood(s)
	if err != nil {
		return err
	}
	if req.URL.String() != rs.Resource {
		return fmt.Errorf("the Robinhood token is only sent to %s, not %s", rs.Resource, req.URL.Redacted())
	}
	exp, _ := r.Expires(s)
	if !r.Now().Before(exp) {
		return fmt.Errorf("the Robinhood access token expired %s; the refresher hasn't run (`ob cred refresh robinhood`), or enroll again", exp.UTC().Format(time.RFC3339))
	}
	req.Header.Set("Authorization", "Bearer "+rs.AccessToken)
	return nil
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
}

func (r *Robinhood) postToken(ctx context.Context, form url.Values) (*tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.TokenURL, bytes.NewBufferString(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := r.HC.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var t tokenResponse
	if resp.StatusCode/100 != 2 || json.Unmarshal(b, &t) != nil || t.AccessToken == "" || t.ExpiresIn <= 0 {
		// The body of a failed token call carries no token; still, report only the status.
		return nil, fmt.Errorf("token endpoint: HTTP %d", resp.StatusCode)
	}
	return &t, nil
}

// Refresh exchanges the refresh token. Robinhood may return a new refresh
// token; whatever comes back is stored.
func (r *Robinhood) Refresh(ctx context.Context, s Secret) (Secret, error) {
	rs, err := parseRobinhood(s)
	if err != nil {
		return nil, err
	}
	// The refresh token goes only to Robinhood's token endpoint, whatever the
	// stored secret says.
	if rs.TokenEndpoint != r.TokenURL {
		return nil, fmt.Errorf("robinhood-oauth names token endpoint %q, not %q; refusing to send the refresh token there", rs.TokenEndpoint, r.TokenURL)
	}
	t, err := r.postToken(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {rs.RefreshToken},
		"client_id":     {rs.ClientID},
		"resource":      {rs.Resource},
	})
	if err != nil {
		return nil, err
	}
	next := *rs
	next.AccessToken = t.AccessToken
	next.AccessExpiresAt = r.Now().Add(time.Duration(t.ExpiresIn) * time.Second).UTC().Format(time.RFC3339)
	if t.RefreshToken != "" {
		next.RefreshToken = t.RefreshToken
	}
	if t.Scope != "" {
		next.Scope = t.Scope
	}
	return json.Marshal(next)
}

// Enroll registers a public client, signs in through the browser (PKCE, with
// a localhost redirect) and keeps the token: spike/rhprobe's flow.
func (r *Robinhood) Enroll(ctx context.Context, ui UI) (Secret, error) {
	ln, err := r.listen()
	if err != nil {
		return nil, err
	}
	defer ln.Close()
	redirect := fmt.Sprintf("http://localhost:%d/callback", ln.Addr().(*net.TCPAddr).Port)

	reg, _ := json.Marshal(map[string]any{
		"client_name":                "options-book (read-only pulls)",
		"redirect_uris":              []string{redirect},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, r.RegisterURL, bytes.NewReader(reg))
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.HC.Do(req)
	if err != nil {
		return nil, fmt.Errorf("register: %w", err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	var client struct {
		ClientID string `json:"client_id"`
	}
	if resp.StatusCode/100 != 2 || json.Unmarshal(body, &client) != nil || client.ClientID == "" {
		return nil, fmt.Errorf("register: HTTP %d", resp.StatusCode)
	}

	verifier, state := randomString(48), randomString(16)
	challenge := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {client.ClientID},
		"redirect_uri":          {redirect},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"},
		"state":                 {state},
		"scope":                 {RobinhoodScope},
		"resource":              {r.Resource},
	}
	authURL := r.AuthorizeURL + "?" + q.Encode()

	codeCh, errCh := make(chan string, 1), make(chan error, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/callback" {
			http.NotFound(w, req)
			return
		}
		switch {
		case req.URL.Query().Get("error") != "":
			fmt.Fprintln(w, "Sign-in was refused. You can close this tab.")
			errCh <- fmt.Errorf("authorization refused: %s", req.URL.Query().Get("error"))
		case req.URL.Query().Get("state") != state:
			fmt.Fprintln(w, "State mismatch. You can close this tab.")
			errCh <- fmt.Errorf("state mismatch on the redirect")
		default:
			fmt.Fprintln(w, "Signed in. You can close this tab.")
			codeCh <- req.URL.Query().Get("code")
		}
	})}
	go srv.Serve(ln)
	defer srv.Shutdown(context.Background())

	ui.Printf("Sign in to Robinhood in the browser. If it doesn't open, paste this URL:\n  %s\n", authURL)
	ui.OpenURL(authURL)
	var code string
	select {
	case code = <-codeCh:
	case err := <-errCh:
		return nil, err
	case <-time.After(r.SignInTimeout):
		return nil, fmt.Errorf("no sign-in within %s", r.SignInTimeout)
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	t, err := r.postToken(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirect},
		"client_id":     {client.ClientID},
		"code_verifier": {verifier},
		"resource":      {r.Resource},
	})
	if err != nil {
		return nil, err
	}
	if t.RefreshToken == "" {
		return nil, fmt.Errorf("Robinhood issued no refresh token; the refresher could not keep this credential alive")
	}
	now := r.Now()
	return json.Marshal(RobinhoodSecret{
		ClientID: client.ClientID, RefreshToken: t.RefreshToken, AccessToken: t.AccessToken,
		AccessExpiresAt: now.Add(time.Duration(t.ExpiresIn) * time.Second).UTC().Format(time.RFC3339),
		TokenEndpoint:   r.TokenURL, Resource: r.Resource, Scope: t.Scope,
		EnrolledAt: now.UTC().Format(time.RFC3339),
	})
}

func randomString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
