# options-book

Deterministic tools for an options **wheel** book (covered calls and cash-secured puts).
**Code computes every figure and holds every piece of state; an agent, if you use one,
orchestrates, judges and explains.**

The wheel-book tools are pure functions of saved broker responses. They never place,
replace or cancel an order and never contact a broker; you place every order yourself.

## Hard rules

- **No order code path** in the wheel-book tools (`cmd/ob`, `internal/strategy/wheel`,
  `internal/ledger`, `internal/history`, `internal/reconcile`). None gets added.
- The optional Options Agent tools (`internal/alpaca`, `internal/risk`, `cmd/agent-tools`) talk to
  Alpaca's **paper** API only; that exception never widens to real money.
- Money is `shopspring/decimal`. Compare with `.Equal` / `.Cmp`, never `==`. Dollar totals from a
  broker must be whole cents (a sub-cent amount is an error, not a rounding). Per-share quotients keep
  10 places; `internal/format` is the only place display rounding happens.
- Go only.
- Any number in a doc or a message is copied from tool output, never estimated.

## Build and test

```sh
go build -o bin/ob ./cmd/ob
go test ./...
```

Needs Go 1.25 or later. The tests read only the **synthetic** repo in `testdata/synthetic`: two
fictional accounts, a few symbols (AAA, BBB, CCC), two sealed snapshots and a generic `rules.json`.
Nothing in this repository comes from a real account.

To run the store tests against a PostgreSQL backend too, point `POSTGRES_DSN` at a database the
test may create schemas in (PostgreSQL 16 locally is enough; each run uses a scratch schema of its
own and removes stale ones). Without it those tests are skipped:

```sh
POSTGRES_DSN="postgres://user@127.0.0.1:5432/dbname?sslmode=disable" go test ./...
```

## Storage

The MCP server keeps its data in **Cloud SQL for PostgreSQL**, the only store (`STORE=cloudsql`, the
default; `STORE=postgres` is for local testing). The store is create-only: the server's database user is
granted `SELECT` and `INSERT` and nothing else, a trigger refuses `UPDATE`, `DELETE` and `TRUNCATE`
for every user, and `store.Open` and `ob db verify` refuse any broken hash, chain or seal. The instance
has no password and no authorised networks; the server reaches it through the Cloud SQL Go connector
with IAM database authentication. `infra/gcp` holds the Terraform, and `docs/cloud-sql-design.md` the
design and the migration runbook.

## Try it on the synthetic data

The commands read `config/`, `books/`, `snapshots/`, `history/` relative to the working directory.

```sh
cd testdata/synthetic
../../bin/ob checkin --snapshot snapshots/2026-02-06      # one defect: an off-rule closer; exits 1
../../bin/ob checkin --snapshot snapshots/2026-02-13      # no defects, one alert
../../bin/ob ledger  --snapshot snapshots/2026-02-13
../../bin/ob book    --snapshot snapshots/2026-02-13
../../bin/ob diff    --from snapshots/2026-02-06 --to snapshots/2026-02-13
../../bin/ob reconcile --snapshot snapshots/2026-02-13 \
  --pnl roth=reconcile/2026-02-13/roth_realized_pnl.json --pnl trad=reconcile/2026-02-13/trad_realized_pnl.json
../../bin/ob ticket  --snapshot snapshots/2026-02-13 --account roth --kind covered_call \
  --option a0000000-0000-4000-8000-000000000021 --limit 1.25
```

`testdata/synthetic/README.md` has every figure worked out by hand. `go run ./tools/synthfixture`
regenerates the whole tree.

## Commands

| Command | What it does |
|---|---|
| `ob ledger` | order history → legs, realized premium, block premium and breakevens |
| `ob checkin` | defects (an order is wrong) and alerts (judgment needed); exits 1 on a defect |
| `ob ticket` | a proposed covered call or put, checked against the rules; exits 1 if rejected |
| `ob book` | the generated wheel-book tables (`--into FILE` splices between markers) |
| `ob snapshot` | checks a snapshot is complete, then fingerprints and seals it |
| `ob diff` | what changed between two snapshots |
| `ob history plan` / `add` | the append-only order log, pulled incrementally |
| `ob expirations` | record how contracts that ran through expiration finished |
| `ob reconcile` | the ledger against the broker's own realized option P&L |
| `ob db` | import, export, verify, migrate and check a store (Cloud SQL, or a local PostgreSQL) |
| `ob cred` | manage the broker credentials the server uses |

The server also offers **`recommend`** (no CLI twin: it pulls a live chain). Given a symbol, it pulls
the call chain in a DTE window (default 14–240 days) and the quotes from Robinhood, stores each
response verbatim, and runs every contract through the same checks as `ticket`. It lists each
contract that passes for every account with free lots, marks the best strike per expiry, and picks
the best on the emptiest ladder rung. Its limit is the midpoint rounded down to the tick, never below
the bid. A below-band delta is kept only at an expiry where nothing in band passes.

The server also keeps the owner's **working notes** (the plan, the bug log, design docs) in a private,
versioned bucket (`NOTES_BUCKET`), outside git. `notes_list` shows them (with `versions`, every
generation), `notes_read` returns one note exactly as stored together with its generation, and
`notes_write` changes one against the generation just read: replace all, append, or replace the one
place `old` occurs. A write is refused if the note has moved since, and nothing is ever deleted, so
every earlier generation can be read back. The tools exist only when the server has a notes bucket;
`infra/gcp/notes_bucket.tf` creates one.

## Accounts, books and rules

`config/accounts.json` lists every account (its label, the last four characters of its number,
the source it is read through, its default book and the order-entry modes it permits). A **book**
(`books/<book>/`) picks a strategy and the accounts it covers (`book.json`) and carries its own
numeric rules (`rules.json`). The files shipped here are examples; every value in
`rules.json` is an illustration, not a recommendation.

## Layout

```
cmd/ob                   the CLI
cmd/ob-mcp               the MCP server (read and write tools over the store)
cmd/agent-tools          the paper-trading agent tools (Alpaca paper only)
internal/ledger          order history → legs, premium, breakevens (pure math)
internal/strategy/wheel  the domain model: Book → Pool → Account → Block → ShortCall/ShortPut → Closer
internal/rules           books/<book>/rules.json, validated on load
internal/config          accounts.json joined with a book.json
internal/snapshot        loads and checks a snapshots/<date>/ folder
internal/history         the append-only order log
internal/diff, reconcile, render, report, format
internal/broker          canonical types and one adapter per source (robinhood, alpaca)
internal/store           the store: the PostgreSQL / Cloud SQL backend, and the in-memory one for tests
internal/layout          where everything lives in the repo layout
testdata/synthetic       the fictional repo the tests read
tools/synthfixture       regenerates it
```

See ARCHITECTURE.md for how the code is built and how to extend it.

## License

Copyright (C) 2026 Branon Martindale.

This program is free software: you can redistribute it and/or modify it under the terms of the
GNU General Public License as published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version. It is distributed in the hope that it will be
useful, but WITHOUT ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or FITNESS
FOR A PARTICULAR PURPOSE. See the [LICENSE](LICENSE) file for the full text.

## Disclaimer

This is software for record-keeping and analysis. **It is not investment advice**, and nothing in
this repository, including the example rules in `books/*/rules.json`, is a recommendation to
buy, sell or hold any security or to follow any strategy. Options trading carries a risk of loss,
including more than you invest. You are responsible for every order you place and for checking
every figure these tools produce against your broker's own records.

Robinhood and Alpaca are trademarks of their respective owners. This project is independent and is
not affiliated with, endorsed by or sponsored by either company.
