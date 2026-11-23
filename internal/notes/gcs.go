package notes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// GCS is a Store over the Cloud Storage JSON API. The bucket must have object
// versioning on: that is what keeps every earlier generation. Client carries
// the credentials (scope devstorage.read_write).
type GCS struct {
	Bucket string
	Client *http.Client
	// Base is the API host; empty means https://storage.googleapis.com.
	Base string
}

// NewGCS returns a Store over bucket using c.
func NewGCS(bucket string, c *http.Client) *GCS { return &GCS{Bucket: bucket, Client: c} }

func (g *GCS) base() string {
	if g.Base != "" {
		return strings.TrimRight(g.Base, "/")
	}
	return "https://storage.googleapis.com"
}

func (g *GCS) objects() string { return g.base() + "/storage/v1/b/" + url.PathEscape(g.Bucket) + "/o" }

// meta is an object's resource, as much of it as the notes need. The API sends
// generation and size as strings.
type meta struct {
	Name        string `json:"name"`
	Generation  string `json:"generation"`
	Size        string `json:"size"`
	Updated     string `json:"updated"`
	TimeDeleted string `json:"timeDeleted"`
}

func (m meta) note() (Note, error) {
	gen, err := strconv.ParseInt(m.Generation, 10, 64)
	if err != nil {
		return Note{}, fmt.Errorf("storage: generation %q: %w", m.Generation, err)
	}
	size, _ := strconv.ParseInt(m.Size, 10, 64)
	up, _ := time.Parse(time.RFC3339Nano, m.Updated)
	return Note{Name: strings.TrimPrefix(m.Name, Prefix), Generation: gen, Size: size, Updated: up, Current: m.TimeDeleted == ""}, nil
}

// do sends req and returns the body (at most limit bytes) and the status.
func (g *GCS) do(req *http.Request, limit int64) ([]byte, int, error) {
	resp, err := g.Client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, 0, err
	}
	if int64(len(b)) > limit {
		return nil, resp.StatusCode, fmt.Errorf("storage: response over %d bytes", limit)
	}
	return b, resp.StatusCode, nil
}

func statusErr(status int, body []byte) error {
	msg := strings.TrimSpace(string(body))
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return fmt.Errorf("storage: HTTP %d: %s", status, msg)
}

func (g *GCS) List(ctx context.Context, versions bool) ([]Note, error) {
	var out []Note
	token := ""
	for {
		q := "prefix=" + url.QueryEscape(Prefix) + "&versions=" + strconv.FormatBool(versions) +
			"&fields=" + url.QueryEscape("items(name,generation,size,updated,timeDeleted),nextPageToken")
		if token != "" {
			q += "&pageToken=" + url.QueryEscape(token)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.objects()+"?"+q, nil)
		if err != nil {
			return nil, err
		}
		b, status, err := g.do(req, 8<<20)
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			return nil, statusErr(status, b)
		}
		var page struct {
			Items         []meta `json:"items"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := json.Unmarshal(b, &page); err != nil {
			return nil, err
		}
		for _, it := range page.Items {
			n, err := it.note()
			if err != nil {
				return nil, err
			}
			if CheckName(n.Name) != nil { // a folder placeholder or a stray object
				continue
			}
			out = append(out, n)
		}
		if page.NextPageToken == "" {
			break
		}
		token = page.NextPageToken
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Generation < out[j].Generation
	})
	return out, nil
}

// Read reads the metadata first, then the bytes pinned to the generation the
// metadata reported, so the body and the number can't disagree.
func (g *GCS) Read(ctx context.Context, name string, generation int64) (*Note, error) {
	if err := CheckName(name); err != nil {
		return nil, err
	}
	obj := g.objects() + "/" + url.PathEscape(Prefix+name)
	q := "?fields=" + url.QueryEscape("name,generation,size,updated,timeDeleted")
	if generation != 0 {
		q += "&generation=" + strconv.FormatInt(generation, 10)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, obj+q, nil)
	if err != nil {
		return nil, err
	}
	b, status, err := g.do(req, 1<<20)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, ErrNotFound
	default:
		return nil, statusErr(status, b)
	}
	var m meta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	n, err := m.note()
	if err != nil {
		return nil, err
	}
	if n.Size > MaxSize {
		return nil, fmt.Errorf("%s is %d bytes, over the %d-byte cap", name, n.Size, MaxSize)
	}
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s?alt=media&generation=%d", obj, n.Generation), nil)
	if err != nil {
		return nil, err
	}
	body, status, err := g.do(req, MaxSize)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, ErrNotFound
	default:
		return nil, statusErr(status, body)
	}
	n.Body = body
	return &n, nil
}

// Write uploads body guarded by ifGenerationMatch. HTTP 412 is a conflict.
func (g *GCS) Write(ctx context.Context, name string, body []byte, ifGeneration int64) (*Note, error) {
	if err := CheckName(name); err != nil {
		return nil, err
	}
	if len(body) > MaxSize {
		return nil, ErrTooLarge
	}
	u := g.base() + "/upload/storage/v1/b/" + url.PathEscape(g.Bucket) + "/o?uploadType=media&name=" + url.QueryEscape(Prefix+name) +
		"&ifGenerationMatch=" + strconv.FormatInt(ifGeneration, 10) +
		"&fields=" + url.QueryEscape("name,generation,size,updated")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "text/markdown; charset=utf-8")
	b, status, err := g.do(req, 1<<20)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusPreconditionFailed:
		ce := &ConflictError{Name: name, If: ifGeneration}
		if cur, err := g.Read(ctx, name, 0); err == nil {
			ce.Current = cur.Generation
		}
		return nil, ce
	default:
		return nil, statusErr(status, b)
	}
	var m meta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	n, err := m.note()
	if err != nil {
		return nil, err
	}
	return &n, nil
}
