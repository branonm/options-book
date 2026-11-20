package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"golang.org/x/oauth2/google"

	"github.com/branonm/options-book/books"
	configfiles "github.com/branonm/options-book/config"
	"github.com/branonm/options-book/internal/layout"
)

// bookFiles is one book's two configuration files.
type bookFiles struct{ Book, Rules []byte }

// configSet is everything the server's configuration is made of: accounts.json
// and every book's book.json and rules.json. Origin says where it came from
// and is logged at startup.
type configSet struct {
	Accounts []byte
	Books    map[string]bookFiles
	Origin   string
}

// embeddedConfig is the configuration compiled into the binary. In the public
// repo these are synthetic; they are what tests and local runs use.
func embeddedConfig() (*configSet, error) {
	cs := &configSet{Accounts: configfiles.Accounts, Books: map[string]bookFiles{}, Origin: "compiled in"}
	for _, name := range books.Names() {
		bb, err := books.Book(name)
		if err != nil {
			return nil, err
		}
		rb, err := books.Rules(name)
		if err != nil {
			return nil, err
		}
		cs.Books[name] = bookFiles{Book: bb, Rules: rb}
	}
	return cs, nil
}

// objectStore is the read-only slice of Cloud Storage the loader needs.
type objectStore interface {
	// List returns the object names under prefix.
	List(ctx context.Context, prefix string) ([]string, error)
	// Read returns an object's bytes and its generation number.
	Read(ctx context.Context, name string) ([]byte, int64, error)
}

// bucketConfig reads the configuration from a bucket laid out as
// accounts.json and books/<book>/{book,rules}.json. It fails closed: a
// missing or unreadable object is an error, never a fallback to the compiled-in
// defaults, so the server cannot start on the wrong accounts or rules.
func bucketConfig(ctx context.Context, objs objectStore, bucket string) (*configSet, error) {
	cs := &configSet{Books: map[string]bookFiles{}}
	gens := []string{}
	read := func(name string) ([]byte, error) {
		b, gen, err := objs.Read(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("config bucket %s: %s: %w", bucket, name, err)
		}
		gens = append(gens, fmt.Sprintf("%s#%d", name, gen))
		return b, nil
	}
	var err error
	if cs.Accounts, err = read(layout.AccountsConfig); err != nil {
		return nil, err
	}
	names, err := objs.List(ctx, layout.BooksDir+"/")
	if err != nil {
		return nil, fmt.Errorf("config bucket %s: list %s/: %w", bucket, layout.BooksDir, err)
	}
	seen := map[string]bool{}
	for _, n := range names {
		parts := strings.Split(strings.TrimPrefix(n, layout.BooksDir+"/"), "/")
		if len(parts) == 2 && (parts[1] == layout.BookFileName || parts[1] == layout.RulesFileName) {
			seen[parts[0]] = true
		}
	}
	for name := range seen {
		var bf bookFiles
		if bf.Book, err = read(layout.BookFile(name)); err != nil {
			return nil, err
		}
		if bf.Rules, err = read(layout.RulesFile(name)); err != nil {
			return nil, err
		}
		cs.Books[name] = bf
	}
	if len(cs.Books) == 0 {
		return nil, fmt.Errorf("config bucket %s: no books under %s/", bucket, layout.BooksDir)
	}
	sort.Strings(gens)
	cs.Origin = "bucket " + bucket + " (" + strings.Join(gens, ", ") + ")"
	return cs, nil
}

// gcsObjects is objectStore over the Cloud Storage JSON API, read-only, using
// the service's default credentials.
type gcsObjects struct {
	bucket string
	c      *http.Client
}

func newGCSObjects(ctx context.Context, bucket string) (*gcsObjects, error) {
	c, err := google.DefaultClient(ctx, "https://www.googleapis.com/auth/devstorage.read_only")
	if err != nil {
		return nil, err
	}
	return &gcsObjects{bucket: bucket, c: c}, nil
}

func (g *gcsObjects) get(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := g.c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("storage: %s", resp.Status)
	}
	return b, nil
}

func (g *gcsObjects) List(ctx context.Context, prefix string) ([]string, error) {
	var out []string
	token := ""
	for {
		u := "https://storage.googleapis.com/storage/v1/b/" + url.PathEscape(g.bucket) + "/o?fields=items(name),nextPageToken&prefix=" + url.QueryEscape(prefix)
		if token != "" {
			u += "&pageToken=" + url.QueryEscape(token)
		}
		b, err := g.get(ctx, u)
		if err != nil {
			return nil, err
		}
		var page struct {
			Items         []struct{ Name string }
			NextPageToken string
		}
		if err := json.Unmarshal(b, &page); err != nil {
			return nil, err
		}
		for _, it := range page.Items {
			out = append(out, it.Name)
		}
		if page.NextPageToken == "" {
			return out, nil
		}
		token = page.NextPageToken
	}
}

func (g *gcsObjects) Read(ctx context.Context, name string) ([]byte, int64, error) {
	base := "https://storage.googleapis.com/storage/v1/b/" + url.PathEscape(g.bucket) + "/o/" + url.PathEscape(name)
	meta, err := g.get(ctx, base+"?fields=generation")
	if err != nil {
		return nil, 0, err
	}
	var m struct {
		Generation json.Number
	}
	if err := json.Unmarshal(meta, &m); err != nil {
		return nil, 0, err
	}
	gen, err := m.Generation.Int64()
	if err != nil {
		return nil, 0, err
	}
	// Pin the read to the generation just reported, so the bytes and the
	// recorded number cannot disagree.
	b, err := g.get(ctx, fmt.Sprintf("%s?alt=media&generation=%d", base, gen))
	return b, gen, err
}
