package cred

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

// fakeRobinhood is Robinhood's OAuth as rhprobe found it: open registration,
// PKCE authorization, a token endpoint that issues and refreshes.
type fakeRobinhood struct {
	t         *testing.T
	srv       *httptest.Server
	mu        sync.Mutex
	challenge map[string]string // code → code_challenge
	refreshes int
	rotateRT  bool // issue a new refresh token on refresh
	failNext  bool
}

func newFakeRobinhood(t *testing.T) *fakeRobinhood {
	f := &fakeRobinhood{t: t, challenge: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRobinhood) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/register":
		var reg map[string]any
		json.NewDecoder(r.Body).Decode(&reg)
		if reg["token_endpoint_auth_method"] != "none" {
			w.WriteHeader(400)
			return
		}
		fmt.Fprint(w, `{"client_id":"client-1"}`)
	case "/oauth":
		q := r.URL.Query()
		if q.Get("client_id") != "client-1" || q.Get("code_challenge_method") != "S256" || q.Get("scope") != RobinhoodScope {
			w.WriteHeader(400)
			return
		}
		f.challenge["code-1"] = q.Get("code_challenge")
		http.Redirect(w, r, q.Get("redirect_uri")+"?code=code-1&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	case "/token":
		r.ParseForm()
		if f.failNext {
			f.failNext = false
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":"invalid_grant"}`)
			return
		}
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if base64.RawURLEncoding.EncodeToString(sum[:]) != f.challenge[r.Form.Get("code")] {
				w.WriteHeader(400)
				return
			}
			fmt.Fprint(w, `{"access_token":"access-0","refresh_token":"refresh-0","expires_in":758797,"scope":"internal"}`)
		case "refresh_token":
			f.refreshes++
			if r.Form.Get("client_id") != "client-1" || !strings.HasPrefix(r.Form.Get("refresh_token"), "refresh-") {
				w.WriteHeader(400)
				return
			}
			rt := ""
			if f.rotateRT {
				rt = fmt.Sprintf(`,"refresh_token":"refresh-%d"`, f.refreshes)
			}
			fmt.Fprintf(w, `{"access_token":"access-%d","expires_in":758797%s}`, f.refreshes, rt)
		}
	}
}

func (f *fakeRobinhood) provider(now *time.Time) *Robinhood {
	p := NewRobinhood(f.srv.Client(), func() time.Time { return *now })
	p.AuthorizeURL, p.TokenURL, p.RegisterURL = f.srv.URL+"/oauth", f.srv.URL+"/token", f.srv.URL+"/register"
	p.SignInTimeout = 5 * time.Second
	return p
}

// browserUI follows the sign-in URL the way a browser would.
type browserUI struct {
	t      *testing.T
	out    strings.Builder
	line   string
	hidden string
}

func (u *browserUI) Printf(format string, a ...any) { fmt.Fprintf(&u.out, format, a...) }
func (u *browserUI) OpenURL(link string) {
	go func() {
		resp, err := http.Get(link)
		if err != nil {
			u.t.Error(err)
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()
}
func (u *browserUI) Line(string) (string, error)   { return u.line, nil }
func (u *browserUI) Hidden(string) (string, error) { return u.hidden, nil }

func enrolled(t *testing.T, f *fakeRobinhood, now *time.Time) (*Robinhood, Secret) {
	t.Helper()
	p := f.provider(now)
	s, err := p.Enroll(context.Background(), &browserUI{t: t})
	if err != nil {
		t.Fatal(err)
	}
	return p, s
}

func TestRobinhoodEnroll(t *testing.T) {
	now := t0
	f := newFakeRobinhood(t)
	p, s := enrolled(t, f, &now)
	var rs RobinhoodSecret
	json.Unmarshal(s, &rs)
	if rs.ClientID != "client-1" || rs.AccessToken != "access-0" || rs.RefreshToken != "refresh-0" || rs.Scope != "internal" ||
		rs.TokenEndpoint != p.TokenURL || rs.Resource != RobinhoodMCP || rs.EnrolledAt != "2026-11-17T12:00:00Z" {
		t.Errorf("secret: %+v", rs)
	}
	exp, ok := p.Expires(s)
	if !ok || !exp.Equal(t0.Add(758797*time.Second)) {
		t.Errorf("expires %v", exp)
	}
}

func TestRobinhoodAuthorize(t *testing.T) {
	now := t0
	f := newFakeRobinhood(t)
	p, s := enrolled(t, f, &now)
	req, _ := http.NewRequest(http.MethodPost, RobinhoodMCP, nil)
	if err := p.Authorize(context.Background(), s, req); err != nil || req.Header.Get("Authorization") != "Bearer access-0" {
		t.Fatalf("authorize: %v %q", err, req.Header.Get("Authorization"))
	}
	other, _ := http.NewRequest(http.MethodPost, "https://example.com/mcp", nil)
	if err := p.Authorize(context.Background(), s, other); err == nil || other.Header.Get("Authorization") != "" {
		t.Error("the token was sent somewhere other than Robinhood's MCP server")
	}
	now = t0.Add(9 * 24 * time.Hour)
	late, _ := http.NewRequest(http.MethodPost, RobinhoodMCP, nil)
	if err := p.Authorize(context.Background(), s, late); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("an expired token was used: %v", err)
	}
	if strings.Contains(fmt.Sprint(p.Authorize(context.Background(), s, late)), "access-0") {
		t.Error("an error message carries the token")
	}
}

func TestRobinhoodRefresh(t *testing.T) {
	now := t0
	f := newFakeRobinhood(t)
	p, s := enrolled(t, f, &now)
	now = t0.Add(7 * 24 * time.Hour)

	// Without a new refresh token, the old one is kept.
	s1, err := p.Refresh(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	var r1 RobinhoodSecret
	json.Unmarshal(s1, &r1)
	if r1.AccessToken != "access-1" || r1.RefreshToken != "refresh-0" || r1.ClientID != "client-1" {
		t.Errorf("after refresh: %+v", r1)
	}
	if exp, _ := p.Expires(s1); !exp.Equal(now.Add(758797 * time.Second)) {
		t.Errorf("expires %v", exp)
	}
	// With one, whatever comes back is stored.
	f.rotateRT = true
	s2, err := p.Refresh(context.Background(), s1)
	if err != nil {
		t.Fatal(err)
	}
	var r2 RobinhoodSecret
	json.Unmarshal(s2, &r2)
	if r2.RefreshToken != "refresh-2" || r2.AccessToken != "access-2" {
		t.Errorf("after refresh with rotation: %+v", r2)
	}
	// The refresh token goes nowhere but the configured token endpoint.
	r2.TokenEndpoint = "https://attacker.example/token"
	bad, _ := json.Marshal(r2)
	if _, err := p.Refresh(context.Background(), bad); err == nil {
		t.Error("refreshed against a foreign token endpoint")
	}
	if f.refreshes != 2 {
		t.Errorf("token endpoint saw %d refreshes, want 2", f.refreshes)
	}
}

func TestRotate(t *testing.T) {
	ctx := context.Background()
	now := t0
	f := newFakeRobinhood(t)
	p, s := enrolled(t, f, &now)
	v := NewMemVault()
	if n, err := Store(ctx, v, p, s); err != nil || n != 1 {
		t.Fatalf("store: %d %v", n, err)
	}
	states := func() string {
		vs, _ := v.List(ctx, "robinhood-oauth")
		var out []string
		for _, x := range vs {
			out = append(out, fmt.Sprintf("%d:%s", x.N, x.State))
		}
		return strings.Join(out, " ")
	}

	// 8.8 days left: nothing to do.
	r, err := Rotate(ctx, v, p, now, false)
	if err != nil || r.Refreshed || r.Version != 1 || f.refreshes != 0 {
		t.Fatalf("early rotate: %+v %v", r, err)
	}
	// Under two days left: refresh, add, disable the previous.
	now = t0.Add(7 * 24 * time.Hour)
	r, err = Rotate(ctx, v, p, now, false)
	if err != nil || !r.Refreshed || r.Version != 2 {
		t.Fatalf("rotate: %+v %v", r, err)
	}
	if got := states(); got != "2:ENABLED 1:DISABLED" {
		t.Errorf("after one rotation: %s", got)
	}
	// Again: the oldest is destroyed, so at most two are kept.
	now = now.Add(7 * 24 * time.Hour)
	if _, err := Rotate(ctx, v, p, now, false); err != nil {
		t.Fatal(err)
	}
	if got := states(); got != "3:ENABLED 2:DISABLED 1:DESTROYED" {
		t.Errorf("after two rotations: %s", got)
	}
	cur, _ := v.Latest(ctx, "robinhood-oauth")
	var rs RobinhoodSecret
	json.Unmarshal(cur.Data, &rs)
	if cur.N != 3 || rs.AccessToken != "access-2" {
		t.Errorf("latest: version %d token %s", cur.N, rs.AccessToken)
	}

	// A failed refresh writes nothing, and the current version stays in use.
	now = now.Add(7 * 24 * time.Hour)
	f.failNext = true
	if _, err := Rotate(ctx, v, p, now, false); err == nil || strings.Contains(err.Error(), "refresh-") || strings.Contains(err.Error(), "access-") {
		t.Errorf("failed refresh: %v", err)
	}
	if got := states(); got != "3:ENABLED 2:DISABLED 1:DESTROYED" {
		t.Errorf("a failed refresh changed the versions: %s", got)
	}
	// force refreshes whatever is left.
	if r, err := Rotate(ctx, v, p, t0, true); err != nil || !r.Refreshed {
		t.Errorf("forced: %+v %v", r, err)
	}
}

func TestDescribeShowsNoSecret(t *testing.T) {
	ctx := context.Background()
	now := t0
	f := newFakeRobinhood(t)
	p, s := enrolled(t, f, &now)
	v := NewMemVault()
	Store(ctx, v, p, s)
	st, err := Describe(ctx, v, p, t0.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	line := st.String()
	b, _ := json.Marshal(st)
	for _, secret := range []string{"access-0", "refresh-0", "client-1"} {
		if strings.Contains(line, secret) || strings.Contains(string(b), secret) {
			t.Errorf("status shows %s: %s %s", secret, line, b)
		}
	}
	if !strings.Contains(line, "version 1") || !strings.Contains(line, "7.8 days left") {
		t.Errorf("status: %s", line)
	}
}

// paperTransport answers for the real paper host, so URLs keep it.
type paperTransport struct {
	seen []string
}

func (p *paperTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	p.seen = append(p.seen, r.URL.Host+r.URL.Path)
	body := `{"id":"acct-1","status":"ACTIVE"}`
	code := 200
	if r.Header.Get("APCA-API-KEY-ID") != "PKTEST" || r.Header.Get("APCA-API-SECRET-KEY") != "s3cret" {
		code, body = 401, `{"message":"unauthorized"}`
	}
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}, Request: r}, nil
}

func TestAlpacaPaper(t *testing.T) {
	ctx := context.Background()
	tr := &paperTransport{}
	a := NewAlpacaPaper(&http.Client{Transport: tr})
	ui := &browserUI{t: t, line: "PKTEST", hidden: "s3cret"}
	s, err := a.Enroll(ctx, ui)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.seen) != 1 || tr.seen[0] != "paper-api.alpaca.markets/v2/account" {
		t.Errorf("verification calls: %v", tr.seen)
	}
	if strings.Contains(ui.out.String(), "s3cret") {
		t.Error("enroll printed the secret key")
	}
	// A wrong key fails verification.
	if _, err := a.Enroll(ctx, &browserUI{t: t, line: "PKTEST", hidden: "wrong"}); err == nil {
		t.Error("an unverified key was enrolled")
	}

	// The paper pin, on every use.
	req, _ := http.NewRequest(http.MethodGet, "https://paper-api.alpaca.markets/v2/positions", nil)
	if err := a.Authorize(ctx, s, req); err != nil || req.Header.Get("APCA-API-KEY-ID") != "PKTEST" {
		t.Errorf("paper request: %v", err)
	}
	for _, u := range []string{"https://api.alpaca.markets/v2/account", "http://paper-api.alpaca.markets/v2/account", "https://paper-api.alpaca.markets.evil.example/v2/account"} {
		live, _ := http.NewRequest(http.MethodGet, u, nil)
		if err := a.Authorize(ctx, s, live); err == nil || live.Header.Get("APCA-API-SECRET-KEY") != "" {
			t.Errorf("the key was sent to %s", u)
		}
	}
	// Market data: GET only, and only that host.
	data, _ := http.NewRequest(http.MethodGet, "https://data.alpaca.markets/v2/stocks/trades/latest?symbols=AAA", nil)
	if err := a.Authorize(ctx, s, data); err != nil || data.Header.Get("APCA-API-KEY-ID") != "PKTEST" {
		t.Errorf("market-data GET: %v", err)
	}
	for _, m := range []string{http.MethodPost, http.MethodDelete, http.MethodPut, http.MethodPatch} {
		bad, _ := http.NewRequest(m, "https://data.alpaca.markets/v2/stocks/trades/latest", nil)
		if err := a.Authorize(ctx, s, bad); err == nil || bad.Header.Get("APCA-API-SECRET-KEY") != "" {
			t.Errorf("the key was sent with %s to the data host", m)
		}
	}
	var as AlpacaPaperSecret
	json.Unmarshal(s, &as)
	for _, ep := range []string{"https://api.alpaca.markets", "https://paper-api.alpaca.markets.evil.example", "http://paper-api.alpaca.markets", ""} {
		as.Endpoint = ep
		bad, _ := json.Marshal(as)
		r, _ := http.NewRequest(http.MethodGet, "https://paper-api.alpaca.markets/v2/account", nil)
		if err := a.Authorize(ctx, bad, r); err == nil {
			t.Errorf("a secret naming %q was used", ep)
		}
	}

	// No refresh, ever.
	if _, err := a.Refresh(ctx, s); !errors.Is(err, ErrNotRefreshable) {
		t.Errorf("refresh: %v", err)
	}
	v := NewMemVault()
	Store(ctx, v, a, s)
	if _, err := Rotate(ctx, v, a, t0, true); !errors.Is(err, ErrNotRefreshable) {
		t.Errorf("rotate: %v", err)
	}
	if st, err := Describe(ctx, v, a, t0); err != nil || strings.Contains(st.String(), "s3cret") || strings.Contains(st.String(), "PKTEST") {
		t.Errorf("status: %v %v", st, err)
	}
}

func TestStoreKeepsTwo(t *testing.T) {
	ctx := context.Background()
	v := NewMemVault()
	a := NewAlpacaPaper(nil)
	for i := 0; i < 3; i++ {
		if _, err := Store(ctx, v, a, Secret(fmt.Sprintf(`{"n":%d}`, i))); err != nil {
			t.Fatal(err)
		}
	}
	vs, _ := v.List(ctx, "alpaca-paper")
	var got []string
	for _, x := range vs {
		got = append(got, fmt.Sprintf("%d:%s", x.N, x.State))
	}
	if strings.Join(got, " ") != "3:ENABLED 2:DISABLED 1:DESTROYED" {
		t.Errorf("after three enrolments: %v", got)
	}
}

// A fake of Secret Manager's REST API, enough for Rotate.
func fakeSecretManager(t *testing.T) *httptest.Server {
	type ver struct {
		data  []byte
		state string
	}
	var mu sync.Mutex
	vers := map[string][]*ver{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer test-token" {
			w.WriteHeader(401)
			return
		}
		p := strings.TrimPrefix(r.URL.Path, "/")
		secret, rest, _ := strings.Cut(p, "/")
		secret, verb, _ := strings.Cut(secret, ":")
		switch {
		case verb == "addVersion":
			var body struct {
				Payload struct{ Data, DataCrc32c string } `json:"payload"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			data, _ := base64.StdEncoding.DecodeString(body.Payload.Data)
			if body.Payload.DataCrc32c == "" {
				w.WriteHeader(400)
				return
			}
			vers[secret] = append(vers[secret], &ver{data, "ENABLED"})
			fmt.Fprintf(w, `{"name":"projects/p/secrets/%s/versions/%d"}`, secret, len(vers[secret]))
		case rest == "versions/latest:access":
			for i := len(vers[secret]) - 1; i >= 0; i-- {
				if vers[secret][i].state == "ENABLED" {
					fmt.Fprintf(w, `{"name":"projects/p/secrets/%s/versions/%d","payload":{"data":%q}}`, secret, i+1, base64.StdEncoding.EncodeToString(vers[secret][i].data))
					return
				}
			}
			w.WriteHeader(404)
		case rest == "versions":
			var out []string
			for i, v := range vers[secret] {
				out = append(out, fmt.Sprintf(`{"name":"projects/p/secrets/%s/versions/%d","state":%q}`, secret, i+1, v.state))
			}
			fmt.Fprintf(w, `{"versions":[%s]}`, strings.Join(out, ","))
		case strings.HasPrefix(rest, "versions/"):
			n, action, _ := strings.Cut(strings.TrimPrefix(rest, "versions/"), ":")
			var i int
			fmt.Sscan(n, &i)
			if i < 1 || i > len(vers[secret]) {
				w.WriteHeader(404)
				return
			}
			vers[secret][i-1].state = map[string]string{"disable": "DISABLED", "destroy": "DESTROYED"}[action]
			fmt.Fprint(w, `{}`)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSecretManagerREST(t *testing.T) {
	ctx := context.Background()
	srv := fakeSecretManager(t)
	v := newSecretManagerAt(srv.URL, srv.Client())
	if _, err := v.Latest(ctx, "robinhood-oauth"); !IsNotExist(err) {
		t.Fatalf("empty secret: %v", err)
	}
	now := t0
	f := newFakeRobinhood(t)
	p, s := enrolled(t, f, &now)
	if n, err := Store(ctx, v, p, s); err != nil || n != 1 {
		t.Fatalf("store: %d %v", n, err)
	}
	now = t0.Add(7 * 24 * time.Hour)
	for i := 0; i < 2; i++ {
		if _, err := Rotate(ctx, v, p, now, true); err != nil {
			t.Fatal(err)
		}
	}
	vs, err := v.List(ctx, "robinhood-oauth")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, x := range vs {
		got = append(got, fmt.Sprintf("%d:%s", x.N, x.State))
	}
	if strings.Join(got, " ") != "3:ENABLED 2:DISABLED 1:DESTROYED" {
		t.Errorf("versions over REST: %v", got)
	}
	cur, err := v.Latest(ctx, "robinhood-oauth")
	if err != nil || cur.N != 3 {
		t.Fatalf("latest: %d %v", cur.N, err)
	}
	if exp, _ := p.Expires(cur.Data); !exp.Equal(now.Add(758797 * time.Second)) {
		t.Errorf("latest expires %v", exp)
	}
}
