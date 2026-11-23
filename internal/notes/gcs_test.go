package notes

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeGCS answers the three calls the client makes and records each request.
type fakeGCS struct {
	t    *testing.T
	reqs []string
	body map[string]string // "name@gen" -> body
	cur  map[string]string // name -> current generation
	list string
	put  func(r *http.Request, body string) (int, string)
}

func (f *fakeGCS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	line := r.Method + " " + r.URL.EscapedPath() + "?" + r.URL.RawQuery
	f.reqs = append(f.reqs, line)
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodPost:
		b, _ := io.ReadAll(r.Body)
		if ct := r.Header.Get("Content-Type"); ct != "text/markdown; charset=utf-8" {
			f.t.Errorf("content type %q", ct)
		}
		code, resp := f.put(r, string(b))
		w.WriteHeader(code)
		io.WriteString(w, resp)
	case r.URL.Path == "/storage/v1/b/bkt/o":
		io.WriteString(w, f.list)
	case strings.HasPrefix(r.URL.EscapedPath(), "/storage/v1/b/bkt/o/notes%2F"):
		name := strings.TrimPrefix(r.URL.EscapedPath(), "/storage/v1/b/bkt/o/notes%2F")
		gen := q.Get("generation")
		if gen == "" {
			gen = f.cur[name]
		}
		b, ok := f.body[name+"@"+gen]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if q.Get("alt") == "media" {
			io.WriteString(w, b)
			return
		}
		del := ""
		if gen != f.cur[name] {
			del = `,"timeDeleted":"2026-11-23T10:00:00Z"`
		}
		io.WriteString(w, `{"name":"notes/`+name+`","generation":"`+gen+`","size":"`+itoa(len(b))+`","updated":"2026-11-23T09:00:00.5Z"`+del+`}`)
	default:
		f.t.Errorf("unexpected %s", line)
		http.NotFound(w, r)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for ; n > 0; n /= 10 {
		s = string(rune('0'+n%10)) + s
	}
	return s
}

func newFake(t *testing.T) (*fakeGCS, *GCS) {
	f := &fakeGCS{t: t, body: map[string]string{}, cur: map[string]string{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, &GCS{Bucket: "bkt", Client: srv.Client(), Base: srv.URL}
}

func TestGCSList(t *testing.T) {
	f, g := newFake(t)
	f.list = `{"items":[
	  {"name":"notes/PLAN.md","generation":"1700000000000002","size":"10","updated":"2026-11-23T09:00:00Z"},
	  {"name":"notes/","generation":"5","size":"0","updated":"2026-11-23T09:00:00Z"},
	  {"name":"notes/BUGS.md","generation":"1700000000000001","size":"7","updated":"2026-11-22T09:00:00Z"},
	  {"name":"notes/PLAN.md","generation":"1700000000000001","size":"4","updated":"2026-11-22T09:00:00Z","timeDeleted":"2026-11-23T09:00:00Z"}]}`
	got, err := g.List(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.reqs) != 1 || f.reqs[0] != "GET /storage/v1/b/bkt/o?prefix=notes%2F&versions=true&fields=items%28name%2Cgeneration%2Csize%2Cupdated%2CtimeDeleted%29%2CnextPageToken" {
		t.Errorf("request: %q", f.reqs)
	}
	// The folder placeholder is skipped; sorted by name then generation.
	if len(got) != 3 || got[0].Name != "BUGS.md" || got[1].Generation != 1700000000000001 || got[1].Current || got[2].Generation != 1700000000000002 || !got[2].Current || got[2].Size != 10 {
		t.Errorf("list: %+v", got)
	}
	if _, err := g.List(context.Background(), false); err != nil || !strings.Contains(f.reqs[1], "versions=false") {
		t.Errorf("versions=false: %v %q", err, f.reqs)
	}
}

func TestGCSRead(t *testing.T) {
	f, g := newFake(t)
	f.cur["PLAN.md"] = "30"
	f.body["PLAN.md@30"] = "new body"
	f.body["PLAN.md@20"] = "old"
	ctx := context.Background()

	n, err := g.Read(ctx, "PLAN.md", 0)
	if err != nil || string(n.Body) != "new body" || n.Generation != 30 || !n.Current {
		t.Fatalf("current: %+v %v", n, err)
	}
	// Metadata first, then the bytes pinned to the generation it reported.
	want := []string{
		"GET /storage/v1/b/bkt/o/notes%2FPLAN.md?fields=name%2Cgeneration%2Csize%2Cupdated%2CtimeDeleted",
		"GET /storage/v1/b/bkt/o/notes%2FPLAN.md?alt=media&generation=30",
	}
	if strings.Join(f.reqs, "|") != strings.Join(want, "|") {
		t.Errorf("requests:\n%q\nwant\n%q", f.reqs, want)
	}
	n, err = g.Read(ctx, "PLAN.md", 20)
	if err != nil || string(n.Body) != "old" || n.Current || !strings.Contains(f.reqs[2], "&generation=20") {
		t.Errorf("old: %+v %v %q", n, err, f.reqs[2:])
	}
	if _, err := g.Read(ctx, "NOPE.md", 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	if _, err := g.Read(ctx, "a/b.md", 0); err == nil {
		t.Error("bad name accepted")
	}
}

func TestGCSWrite(t *testing.T) {
	f, g := newFake(t)
	ctx := context.Background()
	f.put = func(r *http.Request, body string) (int, string) {
		if body != "hello" {
			t.Errorf("body %q", body)
		}
		return 200, `{"name":"notes/PLAN.md","generation":"41","size":"5","updated":"2026-11-23T09:00:00Z"}`
	}
	n, err := g.Write(ctx, "PLAN.md", []byte("hello"), 40)
	if err != nil || n.Generation != 41 || n.Size != 5 {
		t.Fatalf("write: %+v %v", n, err)
	}
	if f.reqs[0] != "POST /upload/storage/v1/b/bkt/o?uploadType=media&name=notes%2FPLAN.md&ifGenerationMatch=40&fields=name%2Cgeneration%2Csize%2Cupdated" {
		t.Errorf("request: %q", f.reqs[0])
	}

	// A create is ifGenerationMatch=0; a 412 is a conflict that names the current generation.
	f.cur["PLAN.md"], f.body["PLAN.md@77"] = "77", "x"
	f.put = func(*http.Request, string) (int, string) { return 412, `{"error":{"code":412}}` }
	_, err = g.Write(ctx, "PLAN.md", []byte("hello"), 0)
	var ce *ConflictError
	if !errors.As(err, &ce) || ce.If != 0 || ce.Current != 77 {
		t.Fatalf("412: %v", err)
	}
	if !strings.Contains(f.reqs[1], "ifGenerationMatch=0") {
		t.Errorf("create request: %q", f.reqs[1])
	}

	f.put = func(*http.Request, string) (int, string) { return 403, "denied" }
	if _, err := g.Write(ctx, "PLAN.md", []byte("hello"), 1); err == nil || strings.Contains(err.Error(), "conflict") || !strings.Contains(err.Error(), "403") {
		t.Errorf("403: %v", err)
	}
	if _, err := g.Write(ctx, "PLAN.md", make([]byte, MaxSize+1), 1); !errors.Is(err, ErrTooLarge) {
		t.Errorf("too large: %v", err)
	}
}
