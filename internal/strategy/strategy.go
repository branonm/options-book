// Package strategy names the strategies a book may run. A book's book.json says
// which one (`"strategy": "wheel"`); config validates the name against Known and
// report.Assemble dispatches on it.
//
// There is deliberately no interface yet. With one strategy, anything bigger is
// a guess; the spreads strategy (item 6 step 5) will show what is generic.
package strategy

// Wheel is the options wheel (internal/strategy/wheel).
const Wheel = "wheel"

// Known lists every strategy id a book may name.
func Known() []string { return []string{Wheel} }
