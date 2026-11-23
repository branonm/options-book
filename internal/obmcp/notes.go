package obmcp

// The owner's working notes (PLAN.md, BUGS.md, design docs), kept in a private
// versioned bucket (internal/notes). notes_read returns a note's body exactly
// and its generation in Result.Generation; notes_write changes it only against
// the generation just read, so two sessions can't overwrite each other, and the
// bucket keeps every earlier generation. There is no delete.

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/branonm/options-book/internal/notes"
)

type NotesListIn struct {
	Versions bool `json:"versions,omitempty" jsonschema:"list every generation of every note, not just the current ones"`
}

type NotesReadIn struct {
	Name       string `json:"name" jsonschema:"the note, e.g. PLAN.md"`
	Generation int64  `json:"generation,omitempty" jsonschema:"an earlier generation to read (from notes_list with versions); default the current one"`
}

type NotesWriteIn struct {
	Name         string  `json:"name" jsonschema:"the note, e.g. PLAN.md: a flat name ending in .md"`
	IfGeneration int64   `json:"if_generation" jsonschema:"the generation you just read with notes_read; 0 creates a note that does not exist yet"`
	Content      *string `json:"content,omitempty" jsonschema:"replace the whole note with this"`
	Append       *string `json:"append,omitempty" jsonschema:"add this to the end of the note (a newline is put first if the note doesn't end in one)"`
	Old          *string `json:"old,omitempty" jsonschema:"text to replace; it must occur exactly once in the note; give it with new"`
	New          *string `json:"new,omitempty" jsonschema:"what to replace old with (empty deletes it)"`
}

func registerNotes(server *mcp.Server, s *tools) {
	mcp.AddTool(server, &mcp.Tool{Name: "notes_list",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: boolp(false)},
		Description: "The owner's working notes (PLAN.md, BUGS.md, design docs): name, generation, bytes and when each was last written. With versions, every generation of every note, the current one marked."},
		expose(s.notesList))
	mcp.AddTool(server, &mcp.Tool{Name: "notes_read",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: boolp(false)},
		Description: "One note's text, exactly as stored and nothing else; its generation is in the result's generation field. Pass that generation to notes_write as if_generation."},
		expose(s.notesRead))
	mcp.AddTool(server, &mcp.Tool{Name: "notes_write",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: boolp(false), IdempotentHint: false, OpenWorldHint: boolp(false)},
		Description: "Change a note against the generation you just read: if_generation is that generation, and the write is refused if the note has moved since (read it again and retry). Give exactly one of content (replace all), append (add to the end), or old with new (replace the one place old occurs; it must occur exactly once). if_generation 0 creates a note that doesn't exist yet. Every earlier generation is kept and nothing is deleted. Returns the new generation."},
		expose(s.notesWrite))
}

func (s *tools) notesStore() (notes.Store, error) {
	if s.d.Notes == nil {
		return nil, fmt.Errorf("notes are not configured on this server")
	}
	return s.d.Notes, nil
}

func (s *tools) notesList(ctx context.Context, _ *mcp.CallToolRequest, in NotesListIn) (*mcp.CallToolResult, Result, error) {
	st, err := s.notesStore()
	if err != nil {
		return nil, Result{}, err
	}
	ns, err := st.List(ctx, in.Versions)
	if err != nil {
		return nil, Result{}, err
	}
	var b bytes.Buffer
	if len(ns) == 0 {
		b.WriteString("No notes.\n")
		return &mcp.CallToolResult{Content: text(&b)}, Result{}, nil
	}
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	if in.Versions {
		fmt.Fprintln(w, "NAME\tGENERATION\tBYTES\tUPDATED\tCURRENT")
	} else {
		fmt.Fprintln(w, "NAME\tGENERATION\tBYTES\tUPDATED")
	}
	for _, n := range ns {
		fmt.Fprintf(w, "%s\t%d\t%d\t%s", n.Name, n.Generation, n.Size, n.Updated.UTC().Format(time.RFC3339))
		if in.Versions {
			cur := ""
			if n.Current {
				cur = "current"
			}
			fmt.Fprintf(w, "\t%s", cur)
		}
		fmt.Fprintln(w)
	}
	w.Flush()
	return &mcp.CallToolResult{Content: text(&b)}, Result{}, nil
}

func (s *tools) notesRead(ctx context.Context, _ *mcp.CallToolRequest, in NotesReadIn) (*mcp.CallToolResult, Result, error) {
	st, err := s.notesStore()
	if err != nil {
		return nil, Result{}, err
	}
	n, err := st.Read(ctx, in.Name, in.Generation)
	if err != nil {
		return nil, Result{}, fmt.Errorf("%s: %w", in.Name, err)
	}
	// The body and nothing else, so it can be edited and written back safely.
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(n.Body)}}},
		Result{Generation: strconv.FormatInt(n.Generation, 10)}, nil
}

func (s *tools) notesWrite(ctx context.Context, _ *mcp.CallToolRequest, in NotesWriteIn) (*mcp.CallToolResult, Result, error) {
	st, err := s.notesStore()
	if err != nil {
		return nil, Result{}, err
	}
	n, err := notes.Change(ctx, st, in.Name, in.IfGeneration, notes.Edit{Content: in.Content, Append: in.Append, Old: in.Old, New: in.New})
	if err != nil {
		return nil, Result{}, err
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "Wrote %s: generation %d, %d bytes.\n", n.Name, n.Generation, n.Size)
	return &mcp.CallToolResult{Content: text(&b)}, Result{Generation: strconv.FormatInt(n.Generation, 10)}, nil
}
