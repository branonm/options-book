package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/branonm/options-book/internal/cred"
)

// The cred CLI over an in-memory vault: status and a refresh that isn't due
// need no network, and nothing printed carries a secret.
func TestCredCLI(t *testing.T) {
	ctx := context.Background()
	v := cred.NewMemVault()
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	oldVault, oldNow := credVault, credNow
	credVault = func(context.Context, string) (cred.Vault, error) { return v, nil }
	credNow = func() time.Time { return now }
	t.Cleanup(func() { credVault, credNow = oldVault, oldNow })

	o := cli(t, runCred, "status", "--project", "p")
	if o.err != nil || !strings.Contains(o.text, "robinhood: not enrolled") || !strings.Contains(o.text, "alpaca-paper: not enrolled") {
		t.Fatalf("status before enrolment: %q %v", o.text, o.err)
	}

	rh, _ := json.Marshal(cred.RobinhoodSecret{
		ClientID: "client-x", AccessToken: "ACCESS-SECRET", RefreshToken: "REFRESH-SECRET",
		AccessExpiresAt: now.Add(8 * 24 * time.Hour).Format(time.RFC3339),
		TokenEndpoint:   cred.RobinhoodToken, Resource: cred.RobinhoodMCP, Scope: "internal",
	})
	ap, _ := json.Marshal(cred.AlpacaPaperSecret{KeyID: "PKKEY", SecretKey: "ALPACA-SECRET", Endpoint: cred.AlpacaPaperEndpoint})
	v.Add(ctx, "robinhood-oauth", rh)
	v.Add(ctx, "alpaca-paper", ap)

	o = cli(t, runCred, "status", "--project", "p")
	if o.err != nil || o.code != 0 || !strings.Contains(o.text, "robinhood-oauth version 1") || !strings.Contains(o.text, "8.0 days left") ||
		!strings.Contains(o.text, "alpaca-paper version 1; does not expire") {
		t.Errorf("status: %q %d %v", o.text, o.code, o.err)
	}
	o2 := cli(t, runCred, "refresh", "robinhood", "--project", "p")
	if o2.err != nil || !strings.Contains(o2.text, "not refreshed") {
		t.Errorf("refresh not due: %q %v", o2.text, o2.err)
	}
	if o3 := cli(t, runCred, "refresh", "alpaca-paper", "--project", "p"); o3.err == nil || o3.code != 1 {
		t.Errorf("alpaca refresh: %+v", o3)
	}
	for _, out := range []string{o.text, o2.text} {
		for _, secret := range []string{"ACCESS-SECRET", "REFRESH-SECRET", "ALPACA-SECRET", "PKKEY", "client-x"} {
			if strings.Contains(out, secret) {
				t.Errorf("printed %s", secret)
			}
		}
	}
	// An expired token makes status fail, so a check can alert on it.
	now = now.Add(9 * 24 * time.Hour)
	if o := cli(t, runCred, "status", "--project", "p"); o.code != 1 {
		t.Errorf("status with an expired token: exit %d", o.code)
	}
}
