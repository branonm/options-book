// Package configfiles compiles config/accounts.json into the ob-mcp server;
// the books are compiled in by package books (the storage design: "rules.json and
// book.json stay files in git, reviewed alongside the the rulebook and compiled
// into the server"). A rule or account change is a commit and a deploy; every
// run records the sha256 of what was in force.
package configfiles

import _ "embed"

//go:embed accounts.json
var Accounts []byte
