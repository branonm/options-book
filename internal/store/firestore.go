package store

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2/google"
)

// Firestore is a DB over Firestore's REST API. It only ever creates
// documents (createDocument fails with ALREADY_EXISTS on a taken id), reads
// one, or lists a collection — no update or delete call exists here.
type Firestore struct {
	root string // .../v1/projects/<p>/databases/<d>/documents
	// userProject is sent as x-goog-user-project, which user credentials need
	// for quota. Only for those: a service account would need
	// serviceusage.services.use to send it, which the insert-only role lacks.
	userProject string
	token       func(ctx context.Context) (string, error)
	hc          *http.Client
}

// NewEmulator talks to a local Firestore emulator (gcloud emulators firestore
// start). Each project id is a separate, initially empty store.
func NewEmulator(host, project string) *Firestore {
	return &Firestore{
		root:  fmt.Sprintf("http://%s/v1/projects/%s/databases/(default)/documents", host, project),
		token: func(context.Context) (string, error) { return "owner", nil },
		hc:    &http.Client{Timeout: 30 * time.Second},
	}
}

// NewFirestore talks to the live database as whoever Application Default
// Credentials name — on the owner's machine, his own account after
// `gcloud auth application-default login`. Imports and exports run this way
// (the storage design: the owner account, from his machine), never as the
// server's insert-only service account.
func NewFirestore(ctx context.Context, project, database string) (*Firestore, error) {
	creds, err := google.FindDefaultCredentials(ctx, "https://www.googleapis.com/auth/datastore")
	if err != nil {
		return nil, fmt.Errorf("Google credentials: %w (run `gcloud auth application-default login`)", err)
	}
	var kind struct {
		Type string `json:"type"`
	}
	if creds.JSON != nil {
		json.Unmarshal(creds.JSON, &kind)
	}
	user := ""
	if kind.Type == "authorized_user" {
		user = project
	}
	return &Firestore{
		root:        fmt.Sprintf("https://firestore.googleapis.com/v1/projects/%s/databases/%s/documents", project, database),
		userProject: user,
		token: func(context.Context) (string, error) {
			t, err := creds.TokenSource.Token()
			if err != nil {
				return "", err
			}
			return t.AccessToken, nil
		},
		hc: &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

func (f *Firestore) do(ctx context.Context, method, u string, body any) (int, []byte, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, r)
	if err != nil {
		return 0, nil, err
	}
	tok, err := f.token(ctx)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if f.userProject != "" {
		req.Header.Set("x-goog-user-project", f.userProject)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := f.hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}

func (f *Firestore) Create(ctx context.Context, coll, id string, d Doc) error {
	if err := checkID(coll, id); err != nil {
		return err
	}
	if err := checkDoc(d); err != nil {
		return fmt.Errorf("%s/%s: %w", coll, id, err)
	}
	fields, err := encodeFields(d)
	if err != nil {
		return fmt.Errorf("%s/%s: %w", coll, id, err)
	}
	u := f.root + "/" + escapePath(coll) + "?documentId=" + url.QueryEscape(id)
	code, b, err := f.do(ctx, http.MethodPost, u, map[string]any{"fields": fields})
	switch {
	case err != nil:
		return fmt.Errorf("%s/%s: %w", coll, id, err)
	case code == http.StatusConflict:
		return fmt.Errorf("%s/%s: %w", coll, id, ErrExists)
	case code != http.StatusOK:
		return fmt.Errorf("%s/%s: create: HTTP %d: %s", coll, id, code, snippet(b))
	}
	return nil
}

func (f *Firestore) Get(ctx context.Context, coll, id string) (Doc, error) {
	code, b, err := f.do(ctx, http.MethodGet, f.root+"/"+escapePath(coll)+"/"+url.PathEscape(id), nil)
	switch {
	case err != nil:
		return nil, fmt.Errorf("%s/%s: %w", coll, id, err)
	case code == http.StatusNotFound:
		return nil, fmt.Errorf("%s/%s: %w", coll, id, fs.ErrNotExist)
	case code != http.StatusOK:
		return nil, fmt.Errorf("%s/%s: get: HTTP %d: %s", coll, id, code, snippet(b))
	}
	var doc restDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	return decodeFields(doc.Fields)
}

type restDoc struct {
	Name   string                     `json:"name"`
	Fields map[string]json.RawMessage `json:"fields"`
}

func (f *Firestore) List(ctx context.Context, coll string) ([]Item, error) {
	var out []Item
	page := ""
	for {
		u := f.root + "/" + escapePath(coll) + "?pageSize=300"
		if page != "" {
			u += "&pageToken=" + url.QueryEscape(page)
		}
		code, b, err := f.do(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", coll, err)
		}
		if code != http.StatusOK {
			return nil, fmt.Errorf("%s: list: HTTP %d: %s", coll, code, snippet(b))
		}
		var resp struct {
			Documents     []restDoc `json:"documents"`
			NextPageToken string    `json:"nextPageToken"`
		}
		if err := json.Unmarshal(b, &resp); err != nil {
			return nil, err
		}
		for _, d := range resp.Documents {
			i := strings.LastIndex(d.Name, "/")
			id, err := url.PathUnescape(d.Name[i+1:])
			if err != nil {
				return nil, err
			}
			fields, err := decodeFields(d.Fields)
			if err != nil {
				return nil, fmt.Errorf("%s/%s: %w", coll, id, err)
			}
			out = append(out, Item{ID: id, Doc: fields})
		}
		if resp.NextPageToken == "" {
			break
		}
		page = resp.NextPageToken
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func snippet(b []byte) string {
	if len(b) > 300 {
		b = b[:300]
	}
	return strings.TrimSpace(string(b))
}

// Firestore value encoding: only the four types Doc allows.
func encodeFields(d Doc) (map[string]any, error) {
	out := make(map[string]any, len(d))
	for k, v := range d {
		switch v := v.(type) {
		case string:
			out[k] = map[string]any{"stringValue": v}
		case int64:
			out[k] = map[string]any{"integerValue": strconv.FormatInt(v, 10)}
		case bool:
			out[k] = map[string]any{"booleanValue": v}
		case []byte:
			out[k] = map[string]any{"bytesValue": base64.StdEncoding.EncodeToString(v)}
		default:
			return nil, fmt.Errorf("field %s: unsupported type %T", k, v)
		}
	}
	return out, nil
}

func decodeFields(m map[string]json.RawMessage) (Doc, error) {
	out := make(Doc, len(m))
	for k, raw := range m {
		var v struct {
			String  *string `json:"stringValue"`
			Integer *string `json:"integerValue"`
			Boolean *bool   `json:"booleanValue"`
			Bytes   *string `json:"bytesValue"`
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("field %s: %w", k, err)
		}
		switch {
		case v.String != nil:
			out[k] = *v.String
		case v.Integer != nil:
			n, err := strconv.ParseInt(*v.Integer, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("field %s: %w", k, err)
			}
			out[k] = n
		case v.Boolean != nil:
			out[k] = *v.Boolean
		case v.Bytes != nil:
			b, err := base64.StdEncoding.DecodeString(*v.Bytes)
			if err != nil {
				return nil, fmt.Errorf("field %s: %w", k, err)
			}
			out[k] = b
		default:
			return nil, fmt.Errorf("field %s: unsupported Firestore value %s", k, snippet(raw))
		}
	}
	return out, nil
}
