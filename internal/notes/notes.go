// Package notes is the owner's working notes (plan, bug log, design docs) kept
// in a private, versioned Cloud Storage bucket and reached only through the
// server's notes tools. Every write is guarded by the generation the caller
// read, so two sessions can't silently overwrite each other, and the bucket
// keeps every earlier generation. There is no delete path.
package notes

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Prefix is where notes live in the bucket.
const Prefix = "notes/"

// MaxSize caps a note's body in bytes.
const MaxSize = 1 << 20

// Note is one generation of one note. Body is set by Read only.
type Note struct {
	Name       string
	Generation int64
	Size       int64
	Updated    time.Time
	Current    bool
	Body       []byte
}

// Store keeps notes. Generation numbers only ever grow for a note.
type Store interface {
	// List returns the current notes, or with versions every generation of
	// every note, ordered by name then generation.
	List(ctx context.Context, versions bool) ([]Note, error)
	// Read returns a note with its body. Generation 0 means the current one.
	Read(ctx context.Context, name string, generation int64) (*Note, error)
	// Write stores body as a new generation. ifGeneration must be the note's
	// current generation, or 0 to create it (and only if it doesn't exist).
	// Anything else is a *ConflictError.
	Write(ctx context.Context, name string, body []byte, ifGeneration int64) (*Note, error)
}

// ErrNotFound means no such note (or no such generation of it).
var ErrNotFound = errors.New("no such note")

// ErrTooLarge means a body over MaxSize.
var ErrTooLarge = fmt.Errorf("note is over %d bytes", MaxSize)

// ConflictError means the note is not at the generation the caller named:
// someone wrote it since, or it already exists, or it doesn't exist yet.
// Re-read and retry.
type ConflictError struct {
	Name string
	// If is the generation the caller guarded the write with.
	If int64
	// Current is the note's current generation, 0 if it has none (or unknown).
	Current int64
}

func (e *ConflictError) Error() string {
	switch {
	case e.If == 0 && e.Current != 0:
		return fmt.Sprintf("%s already exists (generation %d); read it and write against that generation", e.Name, e.Current)
	case e.Current == 0:
		return fmt.Sprintf("%s is not at generation %d; read it again and retry", e.Name, e.If)
	}
	return fmt.Sprintf("%s is at generation %d, not %d; read it again and retry", e.Name, e.Current, e.If)
}

var nameRE = regexp.MustCompile(`^[A-Za-z0-9._-]+\.md$`)

// CheckName accepts a flat name ending in .md made of letters, digits, '.',
// '_' and '-': no slashes, no "..".
func CheckName(name string) error {
	switch {
	case len(name) > 100:
		return fmt.Errorf("note name %q is over 100 characters", name)
	case !nameRE.MatchString(name), strings.Contains(name, ".."), strings.HasPrefix(name, "."):
		return fmt.Errorf("note name %q: want a flat name ending in .md, of letters, digits, '.', '_' and '-' (no '/', no '..', no leading '.')", name)
	}
	return nil
}

// Edit is one change to a note. Set exactly one mode: Content (replace the
// whole note), Append (add to the end), or Old and New (replace the one place
// Old occurs).
type Edit struct {
	Content *string
	Append  *string
	Old     *string
	New     *string
}

// check reports which mode e is in, or why it is not exactly one.
func (e Edit) check() (string, error) {
	var modes []string
	if e.Content != nil {
		modes = append(modes, "content")
	}
	if e.Append != nil {
		modes = append(modes, "append")
	}
	if e.Old != nil || e.New != nil {
		modes = append(modes, "old/new")
	}
	if len(modes) != 1 {
		return "", fmt.Errorf("give exactly one of content, append, or old with new (got %d)", len(modes))
	}
	if modes[0] == "old/new" {
		switch {
		case e.Old == nil:
			return "", errors.New("new needs old: say which text to replace")
		case e.New == nil:
			return "", errors.New("old needs new (empty to delete the text)")
		case *e.Old == "":
			return "", errors.New("old must not be empty")
		}
	}
	return modes[0], nil
}

// apply returns the body that results from editing base.
func (e Edit) apply(mode string, name string, base []byte) ([]byte, error) {
	switch mode {
	case "content":
		return []byte(*e.Content), nil
	case "append":
		out := append([]byte{}, base...)
		if len(out) > 0 && out[len(out)-1] != '\n' {
			out = append(out, '\n')
		}
		return append(out, *e.Append...), nil
	}
	switch n := strings.Count(string(base), *e.Old); n {
	case 0:
		return nil, fmt.Errorf("old text not found in %s", name)
	case 1:
		return []byte(strings.Replace(string(base), *e.Old, *e.New, 1)), nil
	default:
		return nil, fmt.Errorf("old text occurs %d times in %s; it must occur exactly once (include more around it)", n, name)
	}
}

// Change applies edit to the note as it was at ifGeneration and writes the
// result guarded by that same generation. ifGeneration 0 creates the note:
// only Content or Append can do that.
func Change(ctx context.Context, st Store, name string, ifGeneration int64, edit Edit) (*Note, error) {
	if err := CheckName(name); err != nil {
		return nil, err
	}
	mode, err := edit.check()
	if err != nil {
		return nil, err
	}
	var base []byte
	if ifGeneration == 0 {
		if mode == "old/new" {
			return nil, errors.New("old/new edits an existing note: read it and pass its generation as if_generation")
		}
	} else {
		cur, err := st.Read(ctx, name, ifGeneration)
		if errors.Is(err, ErrNotFound) {
			// The generation never existed. Say so only if the note doesn't
			// either; otherwise it is a stale or wrong generation (B10).
			if now, rerr := st.Read(ctx, name, 0); rerr == nil {
				return nil, &ConflictError{Name: name, If: ifGeneration, Current: now.Generation}
			}
			return nil, err
		}
		if err != nil {
			return nil, err
		}
		base = cur.Body
	}
	body, err := edit.apply(mode, name, base)
	if err != nil {
		return nil, err
	}
	if len(body) > MaxSize {
		return nil, ErrTooLarge
	}
	return st.Write(ctx, name, body, ifGeneration)
}
