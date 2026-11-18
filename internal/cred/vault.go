package cred

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2/google"
)

// Version is one secret version.
type Version struct {
	N     int
	State string // ENABLED, DISABLED or DESTROYED
	Data  []byte // only from Latest
}

// Vault is the slice of Secret Manager the credential layer uses.
type Vault interface {
	// Latest returns the newest enabled version, with its data.
	Latest(ctx context.Context, secret string) (Version, error)
	// Add stores a new version and returns its number.
	Add(ctx context.Context, secret string, data []byte) (int, error)
	// List returns every version's number and state, newest first.
	List(ctx context.Context, secret string) ([]Version, error)
	Disable(ctx context.Context, secret string, n int) error
	Destroy(ctx context.Context, secret string, n int) error
}

// MemVault is an in-memory Vault for tests.
type MemVault struct {
	mu   sync.Mutex
	vers map[string][]Version
}

func NewMemVault() *MemVault { return &MemVault{vers: map[string][]Version{}} }

func (m *MemVault) Latest(_ context.Context, secret string) (Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	vs := m.vers[secret]
	for i := len(vs) - 1; i >= 0; i-- {
		if vs[i].State == "ENABLED" {
			return Version{N: vs[i].N, State: "ENABLED", Data: bytes.Clone(vs[i].Data)}, nil
		}
	}
	return Version{}, fmt.Errorf("secret %s has no enabled version: %w", secret, fs.ErrNotExist)
}

func (m *MemVault) Add(_ context.Context, secret string, data []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := len(m.vers[secret]) + 1
	m.vers[secret] = append(m.vers[secret], Version{N: n, State: "ENABLED", Data: bytes.Clone(data)})
	return n, nil
}

func (m *MemVault) List(_ context.Context, secret string) ([]Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Version
	for i := len(m.vers[secret]) - 1; i >= 0; i-- {
		out = append(out, Version{N: m.vers[secret][i].N, State: m.vers[secret][i].State})
	}
	return out, nil
}

func (m *MemVault) set(secret string, n int, state string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.vers[secret] {
		if m.vers[secret][i].N == n {
			if m.vers[secret][i].State == "DESTROYED" {
				return fmt.Errorf("%s version %d is destroyed", secret, n)
			}
			m.vers[secret][i].State = state
			if state == "DESTROYED" {
				m.vers[secret][i].Data = nil
			}
			return nil
		}
	}
	return fmt.Errorf("%s has no version %d", secret, n)
}

func (m *MemVault) Disable(_ context.Context, secret string, n int) error {
	return m.set(secret, n, "DISABLED")
}
func (m *MemVault) Destroy(_ context.Context, secret string, n int) error {
	return m.set(secret, n, "DESTROYED")
}

// SecretManager is a Vault over Secret Manager's REST API, as whoever
// Application Default Credentials name: the owner's account on his machine, a
// service account on Cloud Run. IAM decides what each may do.
type SecretManager struct {
	base        string // https://secretmanager.googleapis.com/v1/projects/<p>/secrets
	userProject string // x-goog-user-project, for user credentials only (as in internal/store)
	token       func() (string, error)
	hc          *http.Client
}

func NewSecretManager(ctx context.Context, project string) (*SecretManager, error) {
	creds, err := google.FindDefaultCredentials(ctx, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		return nil, fmt.Errorf("Google credentials: %w (run `gcloud auth application-default login`)", err)
	}
	var kind struct {
		Type string `json:"type"`
	}
	if creds.JSON != nil {
		json.Unmarshal(creds.JSON, &kind)
	}
	sm := &SecretManager{
		base: "https://secretmanager.googleapis.com/v1/projects/" + project + "/secrets",
		token: func() (string, error) {
			t, err := creds.TokenSource.Token()
			if err != nil {
				return "", err
			}
			return t.AccessToken, nil
		},
		hc: &http.Client{Timeout: 30 * time.Second},
	}
	if kind.Type == "authorized_user" {
		sm.userProject = project
	}
	return sm, nil
}

func (s *SecretManager) do(ctx context.Context, method, u string, body any, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, r)
	if err != nil {
		return err
	}
	tok, err := s.token()
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if s.userProject != "" {
		req.Header.Set("x-goog-user-project", s.userProject)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%s: %w", u[strings.Index(u, "/secrets/")+1:], fs.ErrNotExist)
	case resp.StatusCode/100 != 2:
		// Secret Manager error bodies never contain payload data.
		return fmt.Errorf("secret manager: HTTP %d: %s", resp.StatusCode, clip(b))
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

func versionNumber(name string) (int, error) {
	return strconv.Atoi(name[strings.LastIndex(name, "/")+1:])
}

func (s *SecretManager) Latest(ctx context.Context, secret string) (Version, error) {
	var r struct {
		Name    string `json:"name"`
		Payload struct {
			Data       string `json:"data"`
			DataCrc32c string `json:"dataCrc32c"`
		} `json:"payload"`
	}
	if err := s.do(ctx, http.MethodGet, s.base+"/"+secret+"/versions/latest:access", nil, &r); err != nil {
		return Version{}, err
	}
	data, err := base64.StdEncoding.DecodeString(r.Payload.Data)
	if err != nil {
		return Version{}, err
	}
	if r.Payload.DataCrc32c != "" && r.Payload.DataCrc32c != strconv.FormatUint(uint64(crc32.Checksum(data, crcTable)), 10) {
		return Version{}, fmt.Errorf("secret %s: payload checksum mismatch", secret)
	}
	n, err := versionNumber(r.Name)
	return Version{N: n, State: "ENABLED", Data: data}, err
}

var crcTable = crc32.MakeTable(crc32.Castagnoli)

func (s *SecretManager) Add(ctx context.Context, secret string, data []byte) (int, error) {
	body := map[string]any{"payload": map[string]any{
		"data":       base64.StdEncoding.EncodeToString(data),
		"dataCrc32c": strconv.FormatUint(uint64(crc32.Checksum(data, crcTable)), 10),
	}}
	var r struct {
		Name string `json:"name"`
	}
	if err := s.do(ctx, http.MethodPost, s.base+"/"+secret+":addVersion", body, &r); err != nil {
		return 0, err
	}
	return versionNumber(r.Name)
}

func (s *SecretManager) List(ctx context.Context, secret string) ([]Version, error) {
	var out []Version
	page := ""
	for {
		u := s.base + "/" + secret + "/versions?pageSize=100"
		if page != "" {
			u += "&pageToken=" + page
		}
		var r struct {
			Versions []struct {
				Name  string `json:"name"`
				State string `json:"state"`
			} `json:"versions"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := s.do(ctx, http.MethodGet, u, nil, &r); err != nil {
			return nil, err
		}
		for _, v := range r.Versions {
			n, err := versionNumber(v.Name)
			if err != nil {
				return nil, err
			}
			out = append(out, Version{N: n, State: v.State})
		}
		if r.NextPageToken == "" {
			break
		}
		page = r.NextPageToken
	}
	sort.Slice(out, func(i, j int) bool { return out[i].N > out[j].N })
	return out, nil
}

func (s *SecretManager) Disable(ctx context.Context, secret string, n int) error {
	return s.do(ctx, http.MethodPost, fmt.Sprintf("%s/%s/versions/%d:disable", s.base, secret, n), map[string]any{}, nil)
}

func (s *SecretManager) Destroy(ctx context.Context, secret string, n int) error {
	return s.do(ctx, http.MethodPost, fmt.Sprintf("%s/%s/versions/%d:destroy", s.base, secret, n), map[string]any{}, nil)
}

// IsNotExist reports whether a Vault error means the secret or version is absent.
func IsNotExist(err error) bool { return errors.Is(err, fs.ErrNotExist) }

func clip(b []byte) string {
	if len(b) > 300 {
		b = b[:300]
	}
	return strings.TrimSpace(string(b))
}

// newSecretManagerAt is a SecretManager against another base URL, for tests.
func newSecretManagerAt(base string, hc *http.Client) *SecretManager {
	return &SecretManager{base: base, token: func() (string, error) { return "test-token", nil }, hc: hc}
}
