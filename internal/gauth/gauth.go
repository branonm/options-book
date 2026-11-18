// Package gauth is the access control in front of ob-mcp, as proven by
// the phase 1 spike (the storage design, "Access and security"):
//
//   - /.well-known/oauth-protected-resource[/mcp] serves RFC 9728 metadata
//     naming Google as the authorization server, which Claude's custom
//     connector sign-in reads;
//   - every /mcp request needs a Google access token issued to our OAuth
//     client for the one allowed account (checked with Google's tokeninfo),
//     or it gets 401 with the resource_metadata pointer;
//   - the caller's address is logged against Anthropic's published ranges,
//     never gated (B6: Cloud Run's default ingress reports 0.0.0.0).
package gauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
)

// ErrNotAllowed is a valid token for an account other than the allowed one.
var ErrNotAllowed = errors.New("not the allowed account")

// Anthropic's published outbound ranges for connector traffic:
// https://platform.claude.com/docs/en/api/ip-addresses ("Outbound IP
// addresses"), current as of 2026-11. Logged, never gated (B6).
var anthropicNets = mustCIDRs("160.79.104.0/21", "153.61.208.0/20")

func mustCIDRs(ss ...string) []*net.IPNet {
	nets := make([]*net.IPNet, len(ss))
	for i, s := range ss {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			panic(err)
		}
		nets[i] = n
	}
	return nets
}

// Config is the server's public identity and its one allowed account.
type Config struct {
	ClientID     string // Google OAuth client id Claude signs in with
	AllowedEmail string
	PublicURL    string // https://<service>.run.app, no trailing slash
	ResourceName string // shown by Claude during sign-in
	LocalTesting bool   // skips the https requirement and source logging
}

// Validate requires every field a real deployment needs.
func (c Config) Validate() error {
	var missing []string
	for name, v := range map[string]string{"OAUTH_CLIENT_ID": c.ClientID, "ALLOWED_EMAIL": c.AllowedEmail, "PUBLIC_URL": c.PublicURL} {
		if v == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing environment: %s", strings.Join(missing, ", "))
	}
	if !strings.HasPrefix(c.PublicURL, "https://") && !c.LocalTesting {
		return fmt.Errorf("PUBLIC_URL must be https")
	}
	return nil
}

// Resource is the MCP endpoint URL exactly as it is entered in Claude.
func (c Config) Resource() string { return c.PublicURL + "/mcp" }

func (c Config) metadataURL() string {
	return c.PublicURL + "/.well-known/oauth-protected-resource/mcp"
}

// Verifier decides whether a bearer token belongs to the allowed user.
type Verifier interface {
	Verify(ctx context.Context, token string) (email string, err error)
}

type ctxKey struct{}

// Email is the signed-in account for a request that passed Handler.
func Email(ctx context.Context) string {
	e, _ := ctx.Value(ctxKey{}).(string)
	return e
}

// Handler serves the metadata, a health check, and mcp behind the token check.
func Handler(cfg Config, v Verifier, mcp http.Handler) http.Handler {
	mux := http.NewServeMux()
	meta := func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"resource":                 cfg.Resource(),
			"authorization_servers":    []string{"https://accounts.google.com"},
			"scopes_supported":         []string{"openid", "email"},
			"bearer_methods_supported": []string{"header"},
			"resource_name":            cfg.ResourceName,
		})
	}
	mux.HandleFunc("/.well-known/oauth-protected-resource", meta)
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", meta)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			challenge(w, cfg, "")
			return
		}
		email, err := v.Verify(r.Context(), token)
		if err != nil {
			log.Printf("auth refused: %v", err)
			challenge(w, cfg, "token not accepted")
			return
		}
		if !cfg.LocalTesting {
			logSource(r)
		}
		mcp.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, email)))
	})
	return mux
}

func challenge(w http.ResponseWriter, cfg Config, desc string) {
	h := fmt.Sprintf(`Bearer resource_metadata=%q, scope="openid email"`, cfg.metadataURL())
	if desc != "" {
		h += fmt.Sprintf(`, error="invalid_token", error_description=%q`, desc)
	}
	w.Header().Set("WWW-Authenticate", h)
	writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
}

// logSource records whether the caller's apparent address is in Anthropic's
// published egress ranges. Monitoring only, never a gate (B6).
func logSource(r *http.Request) {
	ip := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0])
	if ip == "" {
		ip, _, _ = net.SplitHostPort(r.RemoteAddr)
	}
	p := net.ParseIP(ip)
	if p == nil {
		log.Printf("source check: unparseable address %q — not blocking", ip)
		return
	}
	for _, n := range anthropicNets {
		if n.Contains(p) {
			return
		}
	}
	log.Printf("source check: %s is not in a published Anthropic egress range — not blocking", p)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
