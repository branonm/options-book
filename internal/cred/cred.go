// Package cred is the credential layer (PLAN.md, "Phase 5 design"): one
// Provider per credential, each stored as versions of one Secret Manager
// secret. Enroll runs once, by the owner, with his own Google sign-in; Refresh
// runs only in the wheel-cred-refresher job; the server only reads the latest
// version and authorizes requests with it. A token is never printed: callers
// report a version number and an expiry.
package cred

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"
)

// Secret is a provider's stored credential, as the bytes of one secret
// version (JSON, in the provider's own shape).
type Secret []byte

// UI is how Enroll talks to the person enrolling.
type UI interface {
	Printf(format string, a ...any)
	OpenURL(u string)
	// Line reads a visible line; Hidden reads one without echo (never argv).
	Line(prompt string) (string, error)
	Hidden(prompt string) (string, error)
}

// Provider is one kind of credential.
type Provider interface {
	// ID names the provider on the command line ("robinhood", "alpaca-paper").
	ID() string
	// Enroll obtains a new credential interactively.
	Enroll(ctx context.Context, ui UI) (Secret, error)
	// Authorize adds the credential to an outgoing request, or refuses it.
	Authorize(ctx context.Context, s Secret, req *http.Request) error
	// Expires is when the credential stops working; ok is false if it doesn't expire.
	Expires(s Secret) (t time.Time, ok bool)
	// Refresh returns a renewed credential, or ErrNotRefreshable.
	Refresh(ctx context.Context, s Secret) (Secret, error)
}

// ErrNotRefreshable is a credential that is replaced by re-enrolling, not refreshed.
var ErrNotRefreshable = errors.New("this credential can't be refreshed; enroll it again")

// SecretName is the Secret Manager secret a provider's versions live in.
func SecretName(providerID string) (string, error) {
	switch providerID {
	case "robinhood":
		return "robinhood-oauth", nil
	case "alpaca-paper":
		return "alpaca-paper", nil
	}
	return "", fmt.Errorf("no credential provider %q (known: robinhood, alpaca-paper)", providerID)
}

// Providers are the known providers, by ID.
func Providers(hc *http.Client, now func() time.Time) map[string]Provider {
	return map[string]Provider{
		"robinhood":    NewRobinhood(hc, now),
		"alpaca-paper": NewAlpacaPaper(hc),
	}
}

// IDs lists the known provider ids, sorted.
func IDs() []string {
	ids := []string{"robinhood", "alpaca-paper"}
	sort.Strings(ids)
	return ids
}

// Status is what may be shown about a credential: never the secret itself.
type Status struct {
	Provider   string     `json:"provider"`
	Secret     string     `json:"secret"`
	Version    int        `json:"version"`
	Expires    *time.Time `json:"expires,omitempty"`
	DaysLeft   *float64   `json:"days_left,omitempty"`
	Refreshing bool       `json:"refreshable"`
}

// Describe reads a credential's latest version and reports its status.
func Describe(ctx context.Context, v Vault, p Provider, now time.Time) (*Status, error) {
	name, err := SecretName(p.ID())
	if err != nil {
		return nil, err
	}
	cur, err := v.Latest(ctx, name)
	if err != nil {
		return nil, err
	}
	st := &Status{Provider: p.ID(), Secret: name, Version: cur.N}
	if exp, ok := p.Expires(cur.Data); ok {
		days := exp.Sub(now).Hours() / 24
		st.Expires, st.DaysLeft, st.Refreshing = &exp, &days, true
	}
	return st, nil
}

// String is one line for a person: provider, version, expiry. No secret.
func (s *Status) String() string {
	if s.Expires == nil {
		return fmt.Sprintf("%s: secret %s version %d; does not expire (re-enroll to replace)", s.Provider, s.Secret, s.Version)
	}
	return fmt.Sprintf("%s: secret %s version %d; access token expires %s (%.1f days left)",
		s.Provider, s.Secret, s.Version, s.Expires.UTC().Format(time.RFC3339), *s.DaysLeft)
}
