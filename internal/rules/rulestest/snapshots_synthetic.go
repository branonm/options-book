package rulestest

import "github.com/branonm/options-book/internal/rules"

// ForSnapshots is the rules the saved snapshots were checked under. The
// synthetic snapshots were made under the book's own rules.json, so r is
// returned unchanged.
func ForSnapshots(r *rules.Rules) *rules.Rules { return r }
