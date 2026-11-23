package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/branonm/options-book/internal/layout"
	"github.com/branonm/options-book/internal/notes"
	"github.com/branonm/options-book/internal/obmcp"
	"github.com/branonm/options-book/internal/store"
)

// notesSession serves the tools over an in-memory MCP connection, with the
// notes store when withNotes is set.
func notesSession(t *testing.T, withNotes bool) (*mcp.ClientSession, *notes.Memory) {
	t.Helper()
	ctx := context.Background()
	db := store.NewMem()
	if _, err := store.ImportRepo(ctx, db, os.DirFS(".")); err != nil {
		t.Fatal(err)
	}
	bks, accts, asha := testBooks(t)
	d := &obmcp.Deps{
		Books: bks, DefaultBook: layout.DefaultBook, Accounts: accts, AccountsSHA: asha,
		DB: db, Spec: memSpec, Commit: "test", CacheTTL: time.Hour,
	}
	mem := notes.NewMemory()
	if withNotes {
		d.Notes = mem
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "options-book", Version: "test"}, &mcp.ServerOptions{Instructions: obmcp.Instructions})
	obmcp.Register(server, d)
	ct, st := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs, mem
}

func TestNotesTools(t *testing.T) {
	cs, mem := notesSession(t, true)
	ctx := context.Background()

	if o := mustTool(t, cs, "notes_list", nil); o.text != "No notes.\n" {
		t.Errorf("empty list: %q", o.text)
	}

	// Create, read back verbatim with its generation, append against it.
	body := "# Plan\n\n- one  \n\ttabbed\n"
	w := mustTool(t, cs, "notes_write", map[string]any{"name": "PLAN.md", "if_generation": 0, "content": body})
	if !strings.Contains(w.text, "Wrote PLAN.md: generation "+w.generation+", 24 bytes.") || w.generation == "" {
		t.Fatalf("create: %+v", w)
	}
	r := mustTool(t, cs, "notes_read", map[string]any{"name": "PLAN.md"})
	if r.text != body || r.generation != w.generation {
		t.Fatalf("read: %q gen %s; want %q gen %s", r.text, r.generation, body, w.generation)
	}
	a := mustTool(t, cs, "notes_write", map[string]any{"name": "PLAN.md", "if_generation": mustInt(t, r.generation), "append": "- two\n"})
	r2 := mustTool(t, cs, "notes_read", map[string]any{"name": "PLAN.md"})
	if r2.text != body+"- two\n" || r2.generation != a.generation || a.generation == r.generation {
		t.Fatalf("append: %q gen %s", r2.text, r2.generation)
	}

	// old/new, and a deletion with an empty new.
	b := mustTool(t, cs, "notes_write", map[string]any{"name": "PLAN.md", "if_generation": mustInt(t, r2.generation), "old": "- two", "new": "- 2"})
	mustTool(t, cs, "notes_write", map[string]any{"name": "PLAN.md", "if_generation": mustInt(t, b.generation), "old": "\ttabbed\n", "new": ""})
	if got, _ := mem.Read(ctx, "PLAN.md", 0); string(got.Body) != "# Plan\n\n- one  \n- 2\n" {
		t.Errorf("old/new: %q", got.Body)
	}

	// An earlier generation is still readable, and listing shows every one.
	if old := mustTool(t, cs, "notes_read", map[string]any{"name": "PLAN.md", "generation": mustInt(t, r.generation)}); old.text != body {
		t.Errorf("old generation: %q", old.text)
	}
	l := mustTool(t, cs, "notes_list", map[string]any{"versions": true})
	if n := strings.Count(l.text, "PLAN.md"); n != 4 || strings.Count(l.text, "current") != 1 { // only the last row is current (the header says CURRENT)
		t.Errorf("versions list:\n%s", l.text)
	}
	if l := mustTool(t, cs, "notes_list", nil); strings.Count(l.text, "PLAN.md") != 1 {
		t.Errorf("current list:\n%s", l.text)
	}
}

func TestNotesRefusals(t *testing.T) {
	cs, _ := notesSession(t, true)
	g := mustTool(t, cs, "notes_write", map[string]any{"name": "A.md", "if_generation": 0, "content": "x\n"}).generation

	for _, c := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"notes_write", map[string]any{"name": "A.md", "if_generation": 0, "content": "again"}, "already exists"},
		{"notes_write", map[string]any{"name": "A.md", "if_generation": 5, "append": "y"}, "no such note"},
		{"notes_write", map[string]any{"name": "../A.md", "if_generation": 0, "content": "x"}, "note name"},
		{"notes_write", map[string]any{"name": "A.md", "if_generation": mustInt(t, g)}, "exactly one"},
		{"notes_write", map[string]any{"name": "A.md", "if_generation": mustInt(t, g), "old": "zzz", "new": "q"}, "not found"},
		{"notes_read", map[string]any{"name": "NOPE.md"}, "no such note"},
		{"notes_read", map[string]any{"name": "a/b.md"}, "note name"},
	} {
		if o := callTool(t, cs, c.tool, c.args); !o.isError || !strings.Contains(o.text, c.want) {
			t.Errorf("%s %v: error=%v %q, want %q", c.tool, c.args, o.isError, o.text, c.want)
		}
	}

	// A stale generation is refused and writes nothing.
	mustTool(t, cs, "notes_write", map[string]any{"name": "A.md", "if_generation": mustInt(t, g), "append": "two\n"})
	o := callTool(t, cs, "notes_write", map[string]any{"name": "A.md", "if_generation": mustInt(t, g), "append": "other\n"})
	if !o.isError || !strings.Contains(o.text, "read it again") {
		t.Errorf("stale write: %v %q", o.isError, o.text)
	}
	if r := mustTool(t, cs, "notes_read", map[string]any{"name": "A.md"}); r.text != "x\ntwo\n" {
		t.Errorf("body after refused write: %q", r.text)
	}
}

func TestNotesToolsAbsentWithoutStore(t *testing.T) {
	cs, _ := notesSession(t, false)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range res.Tools {
		if strings.HasPrefix(tl.Name, "notes_") {
			t.Errorf("%s offered with no notes store", tl.Name)
		}
	}
	cs, _ = notesSession(t, true)
	res, _ = cs.ListTools(context.Background(), nil)
	n := 0
	for _, tl := range res.Tools {
		if strings.HasPrefix(tl.Name, "notes_") {
			n++
		}
	}
	if n != 3 {
		t.Errorf("%d notes tools, want 3", n)
	}
}

func mustInt(t *testing.T, s string) int64 {
	t.Helper()
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			t.Fatalf("generation %q", s)
		}
		n = n*10 + int64(c-'0')
	}
	return n
}
