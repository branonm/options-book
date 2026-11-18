package gauth

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Copied unchanged from spike/gcpping, where it passed phase 1 against live
// Claude sign-ins (11/16); only the names are exported.

// GoogleVerifier checks a Google OAuth access token with Google's tokeninfo
// endpoint. Google's access tokens are opaque, so asking Google is the only
// way to learn which client and which account a token belongs to.
type GoogleVerifier struct {
	clientID     string
	allowedEmail string
	endpoint     string // tokeninfo URL; replaced in tests
	hc           *http.Client
	now          func() time.Time

	mu    sync.Mutex
	cache map[[32]byte]cached
}

type cached struct {
	email   string
	expires time.Time
}

func NewGoogleVerifier(clientID, allowedEmail string) *GoogleVerifier {
	return &GoogleVerifier{
		clientID:     clientID,
		allowedEmail: strings.ToLower(allowedEmail),
		endpoint:     "https://oauth2.googleapis.com/tokeninfo",
		hc:           &http.Client{Timeout: 5 * time.Second},
		now:          time.Now,
		cache:        map[[32]byte]cached{},
	}
}

func (v *GoogleVerifier) Verify(ctx context.Context, token string) (string, error) {
	key := sha256.Sum256([]byte(token))
	v.mu.Lock()
	if c, ok := v.cache[key]; ok && v.now().Before(c.expires) {
		v.mu.Unlock()
		return c.email, nil
	}
	v.mu.Unlock()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, v.endpoint+"?access_token="+url.QueryEscape(token), nil)
	resp, err := v.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("tokeninfo: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("tokeninfo: HTTP %d", resp.StatusCode)
	}
	var ti struct {
		Aud           string `json:"aud"`
		Azp           string `json:"azp"`
		Email         string `json:"email"`
		EmailVerified string `json:"email_verified"`
		ExpiresIn     string `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ti); err != nil {
		return "", fmt.Errorf("tokeninfo: %w", err)
	}
	if ti.Aud != v.clientID && ti.Azp != v.clientID {
		return "", fmt.Errorf("token was issued to another client")
	}
	if ti.EmailVerified != "true" {
		return "", fmt.Errorf("email not verified")
	}
	email := strings.ToLower(ti.Email)
	if email != v.allowedEmail {
		return "", ErrNotAllowed
	}
	secs, err := strconv.Atoi(ti.ExpiresIn)
	if err != nil || secs <= 0 {
		return "", fmt.Errorf("token expired")
	}
	v.mu.Lock()
	v.cache[key] = cached{email: email, expires: v.now().Add(time.Duration(secs) * time.Second)}
	v.mu.Unlock()
	return email, nil
}
