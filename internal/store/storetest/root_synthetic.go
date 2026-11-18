package storetest

import "path/filepath"

// DataRoot is the synthetic repo root: the default for every test, so the
// public repo's tests never read a real account.
func DataRoot(module string) string { return filepath.Join(module, "testdata", "synthetic") }
