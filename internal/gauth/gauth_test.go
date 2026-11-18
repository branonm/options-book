package gauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	testClient = "client-123.apps.googleusercontent.com"
	testEmail  = "owner@example.com"
	goodToken  = "good-token"
)

type fakeVerifier struct{}

func (fakeVerifier) Verify(_ context.Context, tok string) (string, error) {
	switch tok {
	case goodToken:
		return testEmail, nil
	case "stranger":
		return "", ErrNotAllowed
	}
	return "", fmt.Errorf("tokeninfo: HTTP 400")
}

var testCfg = Config{ClientID: testClient, AllowedEmail: testEmail, PublicURL: "https://wheel.example.run.app", ResourceName: "options-book"}

// echo answers with the signed-in email, so the test sees what reached the MCP handler.
var echo = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, Email(r.Context())) })

func do(t *testing.T, from, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}"))
	req.Header.Set("X-Forwarded-For", from)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	Handler(testCfg, fakeVerifier{}, echo).ServeHTTP(w, req)
	return w
}

func TestConfigValidate(t *testing.T) {
	if err := (Config{}).Validate(); err == nil || !strings.Contains(err.Error(), "OAUTH_CLIENT_ID") {
		t.Errorf("empty config: %v", err)
	}
	c := testCfg
	c.PublicURL = "http://wheel.example"
	if c.Validate() == nil {
		t.Error("http PUBLIC_URL accepted")
	}
	if testCfg.Validate() != nil {
		t.Error("good config refused")
	}
}

func TestResourceMetadata(t *testing.T) {
	for _, p := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		w := httptest.NewRecorder()
		Handler(testCfg, fakeVerifier{}, echo).ServeHTTP(w, httptest.NewRequest(http.MethodGet, p, nil))
		var m map[string]any
		json.Unmarshal(w.Body.Bytes(), &m)
		// Claude requires resource to match the MCP URL exactly.
		if m["resource"] != "https://wheel.example.run.app/mcp" || w.Code != 200 {
			t.Errorf("%s: %d %v", p, w.Code, m)
		}
	}
}

func TestUnauthenticatedGets401WithMetadataPointer(t *testing.T) {
	for _, tok := range []string{"", "garbage", "stranger"} {
		w := do(t, "160.79.104.10", tok)
		if w.Code != http.StatusUnauthorized || !strings.Contains(w.Header().Get("WWW-Authenticate"), `resource_metadata="https://wheel.example.run.app/.well-known/oauth-protected-resource/mcp"`) {
			t.Errorf("token %q: %d %q", tok, w.Code, w.Header().Get("WWW-Authenticate"))
		}
		if w.Body.String() == testEmail {
			t.Errorf("token %q reached the MCP handler", tok)
		}
	}
}

// B6: the source address is logged, never a gate.
func TestSourceIsNeverAGate(t *testing.T) {
	for _, from := range []string{"0.0.0.0", "203.0.113.9", "160.79.104.10", ""} {
		if w := do(t, from, goodToken); w.Code != 200 || w.Body.String() != testEmail {
			t.Errorf("from %q: %d %q", from, w.Code, w.Body)
		}
	}
}

func TestGoogleVerifier(t *testing.T) {
	calls := 0
	ti := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch r.URL.Query().Get("access_token") {
		case "ok":
			fmt.Fprintf(w, `{"aud":%q,"email":"Owner@Example.com","email_verified":"true","expires_in":"3599"}`, testClient)
		case "other-client":
			fmt.Fprint(w, `{"aud":"someone-else","email":"owner@example.com","email_verified":"true","expires_in":"3599"}`)
		case "other-user":
			fmt.Fprintf(w, `{"aud":%q,"email":"stranger@example.com","email_verified":"true","expires_in":"3599"}`, testClient)
		case "unverified":
			fmt.Fprintf(w, `{"aud":%q,"email":"owner@example.com","email_verified":"false","expires_in":"3599"}`, testClient)
		default:
			w.WriteHeader(400)
		}
	}))
	defer ti.Close()
	v := NewGoogleVerifier(testClient, testEmail)
	v.endpoint = ti.URL
	now := time.Date(2026, 11, 10, 18, 0, 0, 0, time.UTC)
	v.now = func() time.Time { return now }

	if e, err := v.Verify(context.Background(), "ok"); err != nil || e != testEmail {
		t.Fatalf("ok: %q %v", e, err)
	}
	v.Verify(context.Background(), "ok")
	if calls != 1 {
		t.Errorf("second check of a cached token called Google (%d calls)", calls)
	}
	now = now.Add(time.Hour)
	v.Verify(context.Background(), "ok")
	if calls != 2 {
		t.Errorf("expired cache entry not re-checked (%d calls)", calls)
	}
	for _, tok := range []string{"other-client", "other-user", "unverified", "garbage"} {
		if _, err := v.Verify(context.Background(), tok); err == nil {
			t.Errorf("%s: accepted", tok)
		}
	}
}
