package notes

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func sp(s string) *string { return &s }

func TestCheckName(t *testing.T) {
	for _, ok := range []string{"PLAN.md", "item6-step3-design.md", "a_b.c.md", "x.md"} {
		if err := CheckName(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "PLAN", "PLAN.txt", ".md", "a/b.md", "../a.md", "a..b.md", "a b.md", "notes/PLAN.md", ".hidden.md", "é.md", strings.Repeat("a", 98) + ".md"} {
		if CheckName(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestCreateReadEdit(t *testing.T) {
	ctx := context.Background()
	st := NewMemory()

	n, err := Change(ctx, st, "PLAN.md", 0, Edit{Content: sp("# Plan\nline one\n")})
	if err != nil || n.Generation == 0 || n.Size != 16 {
		t.Fatalf("create: %+v %v", n, err)
	}
	g1 := n.Generation

	// append adds a newline first when the note doesn't end in one.
	n, err = Change(ctx, st, "PLAN.md", g1, Edit{Append: sp("two")})
	if err != nil {
		t.Fatal(err)
	}
	n, err = Change(ctx, st, "PLAN.md", n.Generation, Edit{Append: sp("three\n")})
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.Read(ctx, "PLAN.md", 0)
	if err != nil || string(got.Body) != "# Plan\nline one\ntwo\nthree\n" || !got.Current {
		t.Fatalf("after appends: %q %v", got.Body, err)
	}

	// old/new replaces the one place; delete with an empty new.
	n, err = Change(ctx, st, "PLAN.md", got.Generation, Edit{Old: sp("two"), New: sp("2")})
	if err != nil {
		t.Fatal(err)
	}
	n, err = Change(ctx, st, "PLAN.md", n.Generation, Edit{Old: sp("three\n"), New: sp("")})
	if err != nil {
		t.Fatal(err)
	}
	got, _ = st.Read(ctx, "PLAN.md", 0)
	if string(got.Body) != "# Plan\nline one\n2\n" {
		t.Errorf("after old/new: %q", got.Body)
	}

	// content replaces everything; an empty note is allowed.
	if _, err = Change(ctx, st, "PLAN.md", got.Generation, Edit{Content: sp("")}); err != nil {
		t.Fatal(err)
	}
	got, _ = st.Read(ctx, "PLAN.md", 0)
	if len(got.Body) != 0 {
		t.Errorf("content: %q", got.Body)
	}
}

func TestEditErrors(t *testing.T) {
	ctx := context.Background()
	st := NewMemory()
	n, _ := Change(ctx, st, "A.md", 0, Edit{Content: sp("x y x\n")})
	g := n.Generation
	for _, c := range []struct {
		name string
		gen  int64
		e    Edit
		want string
	}{
		{"A.md", g, Edit{Old: sp("zzz"), New: sp("q")}, "not found"},
		{"A.md", g, Edit{Old: sp("x"), New: sp("q")}, "2 times"},
		{"A.md", g, Edit{New: sp("q")}, "new needs old"},
		{"A.md", g, Edit{Old: sp("x")}, "old needs new"},
		{"A.md", g, Edit{Old: sp(""), New: sp("q")}, "must not be empty"},
		{"A.md", g, Edit{}, "exactly one"},
		{"A.md", g, Edit{Content: sp("a"), Append: sp("b")}, "exactly one"},
		{"B.md", 0, Edit{Old: sp("x"), New: sp("q")}, "existing note"},
		{"B.md", 12345, Edit{Append: sp("x")}, "no such note"},
		{"../B.md", 0, Edit{Content: sp("x")}, "note name"},
		{"B.md", 0, Edit{Content: sp(strings.Repeat("x", MaxSize+1))}, "over"},
	} {
		_, err := Change(ctx, st, c.name, c.gen, c.e)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s gen %d: err = %v, want %q", c.name, c.gen, err, c.want)
		}
	}
	// None of those wrote anything.
	if l, _ := st.List(ctx, true); len(l) != 1 {
		t.Errorf("%d generations stored, want 1", len(l))
	}
}

func TestConflicts(t *testing.T) {
	ctx := context.Background()
	st := NewMemory()
	n, _ := Change(ctx, st, "A.md", 0, Edit{Content: sp("one\n")})
	g1 := n.Generation

	// A create when the note exists.
	_, err := Change(ctx, st, "A.md", 0, Edit{Content: sp("again")})
	var ce *ConflictError
	if !errors.As(err, &ce) || ce.Current != g1 || ce.If != 0 || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("create over existing: %v", err)
	}

	// Two sessions read g1; the first writes, the second is stale.
	n2, err := Change(ctx, st, "A.md", g1, Edit{Append: sp("two\n")})
	if err != nil {
		t.Fatal(err)
	}
	_, err = Change(ctx, st, "A.md", g1, Edit{Append: sp("other\n")})
	if !errors.As(err, &ce) || ce.Current != n2.Generation || ce.If != g1 {
		t.Fatalf("stale write: %v", err)
	}
	got, _ := st.Read(ctx, "A.md", 0)
	if string(got.Body) != "one\ntwo\n" {
		t.Errorf("body after refused write: %q", got.Body)
	}
}

func TestVersionsKept(t *testing.T) {
	ctx := context.Background()
	st := NewMemory()
	n, _ := Change(ctx, st, "A.md", 0, Edit{Content: sp("v1")})
	g1 := n.Generation
	n, _ = Change(ctx, st, "A.md", g1, Edit{Content: sp("v2")})
	g2 := n.Generation
	Change(ctx, st, "B.md", 0, Edit{Content: sp("b")})

	cur, _ := st.List(ctx, false)
	if len(cur) != 2 || cur[0].Name != "A.md" || cur[0].Generation != g2 || cur[1].Name != "B.md" {
		t.Errorf("current: %+v", cur)
	}
	all, _ := st.List(ctx, true)
	if len(all) != 3 || all[0].Generation != g1 || all[0].Current || all[1].Generation != g2 || !all[1].Current {
		t.Errorf("all: %+v", all)
	}
	old, err := st.Read(ctx, "A.md", g1)
	if err != nil || string(old.Body) != "v1" || old.Current {
		t.Errorf("old generation: %+v %v", old, err)
	}
	if _, err := st.Read(ctx, "A.md", 9); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown generation: %v", err)
	}
	if _, err := st.Read(ctx, "C.md", 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown note: %v", err)
	}
	// Editing from an old generation reads that text but is refused as stale.
	if _, err := Change(ctx, st, "A.md", g1, Edit{Append: sp("x")}); err == nil {
		t.Error("write against a stale generation succeeded")
	}
}
