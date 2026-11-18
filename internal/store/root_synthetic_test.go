package store

import "path/filepath"

// repoRoot is the repo the tests import and read: the synthetic one by default.
func repoRoot() string { return filepath.Join("..", "..", "testdata", "synthetic") }
