// Package books compiles every book (books/<book>/{book,rules}.json) into the
// ob-mcp server. See package configfiles for accounts.json.
package books

import (
	"embed"
	"io/fs"
	"sort"
)

//go:embed */book.json */rules.json
var files embed.FS

// Names lists the compiled-in books, sorted.
func Names() []string {
	ents, _ := fs.ReadDir(files, ".")
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// Book is a book's book.json, as compiled in.
func Book(name string) ([]byte, error) { return fs.ReadFile(files, name+"/book.json") }

// Rules is a book's rules.json, as compiled in.
func Rules(name string) ([]byte, error) { return fs.ReadFile(files, name+"/rules.json") }
