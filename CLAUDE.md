# options-book: context for Claude Code

Deterministic tools for an options wheel book. **Code computes every figure and holds every
piece of state; the agent orchestrates, judges and explains.** See README.md for the commands and
ARCHITECTURE.md for how the code is built and how to extend it; read the latter before changing code.

## Hard rules

- The wheel-book tools (`cmd/ob`, `internal/strategy/wheel`, `internal/ledger`, `internal/history`,
  `internal/reconcile`) are pure functions of saved broker responses. **Never add an order code
  path**, and never contact a broker from them.
- `internal/alpaca`, `internal/risk`, `cmd/agent-tools` are the Options Agent's tools and contact
  Alpaca's **paper** API only. That exception never widens to real money or another broker.
- **Money is `shopspring/decimal`.** Compare with `.Equal` / `.Cmp`, never `==`. Dollar totals from
  a broker must be whole cents. `internal/format` is the only place display rounding happens.
- **Go only.** No Python anywhere in this repo.
- Any number in a doc or a message is copied from tool output.
- `orders.jsonl` in an order log is append-only; a sealed snapshot (one with `manifest.json`) is
  read-only.
- Rule numbers live in the book's `rules.json`, validated on load.
- Tests read only the synthetic repo in `testdata/synthetic`. Never commit a real account, holding,
  fill or credential.

## Build and test

```sh
go build -o bin/ob ./cmd/ob && go test ./...
go run ./tools/synthfixture -ob bin/ob -out testdata/synthetic   # regenerate the synthetic repo
```

A **defect** is a problem with an order in a broker account (found by `ob checkin`, fixed at the
broker). A **tool bug** is a mistake in this code, fixed by a commit with a regression test.
