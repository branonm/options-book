package alpaca

import (
	"testing"

	"github.com/branonm/options-book/internal/cred"
)

func TestConfigFromEnv(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		t.Setenv("APCA_API_KEY_ID", "")
		t.Setenv("APCA_API_SECRET_KEY", "")
		if _, err := ConfigFromEnv(); err == nil {
			t.Fatal("expected an error when credentials are unset")
		}
	})

	t.Run("present", func(t *testing.T) {
		t.Setenv("APCA_API_KEY_ID", "key123")
		t.Setenv("APCA_API_SECRET_KEY", "secret123")
		cfg, err := ConfigFromEnv()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.APIKeyID != "key123" || cfg.APISecretKey != "secret123" {
			t.Fatalf("got %+v", cfg)
		}
	})
}

func TestNewClientRequiresCredentials(t *testing.T) {
	cases := []Config{
		{},
		{APIKeyID: "key123"},
		{APISecretKey: "secret123"},
	}
	for _, cfg := range cases {
		if _, err := NewClient(cfg); err == nil {
			t.Fatalf("expected an error for incomplete config %+v", cfg)
		}
	}
}

func TestNewClientBuildsBothSubClients(t *testing.T) {
	c, err := NewClient(Config{APIKeyID: "key123", APISecretKey: "secret123"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.trading == nil || c.data == nil {
		t.Fatal("expected both trading and data clients to be initialized")
	}
}

// The credential layer pins the same paper endpoint this client does.
func TestPaperEndpointMatchesCred(t *testing.T) {
	if paperBaseURL != cred.AlpacaPaperEndpoint {
		t.Fatalf("internal/alpaca pins %s, internal/cred pins %s", paperBaseURL, cred.AlpacaPaperEndpoint)
	}
}
