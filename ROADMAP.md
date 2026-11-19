# Roadmap

What this project does not do yet. Nothing here is a commitment or a schedule, and all of it is open
to contributions: open an issue first so we can agree on the shape. Each item points at the code that
shows the gap.

## Multi-leg strategies (spreads)

The ledger accepts only single-leg sell-to-open and buy-to-close orders; anything else is an error
rather than a guess (ARCHITECTURE.md, "Invariants", item 4). `internal/strategy` has no interface on purpose:
with one strategy (`wheel`), any interface would be a guess. A second strategy, such as defined-risk
spreads, is what would show what is generic. Open questions: where block premium and breakevens
belong (today in `internal/strategy/wheel`), and how `internal/risk` (position caps for proposals the
wheel ticket does not cover) fits in.

A spread monitor would sit on top of that: cost to close at mid and at the natural price against a
trigger, plus entry gates, for an account that holds spreads. It needs the multi-leg ledger first.

## Placed order versus ticket

`ob ticket` proposes an order and its closer, but nothing checks that the order that was later
placed is the one proposed. The idea: a create-only `ticket` record (id, account, kind, legs by OCC
symbol, side and effect, quantity, limit, time in force, the closer, the modes offered, creation time
and the sha256 of its inputs). `ob checkin` would then match each new order in the log to a ticket
(same account, contract and side/effect, created after the ticket). Any difference in contract,
quantity, limit or time in force, or a missing closer, is a defect; an order with no ticket is an
alert. It must work on every entry path, so hand-entry mistakes are caught too. On a source whose
guarantees do not include open orders the check reports `cannot_verify`, never a pass.

Related and already built: an account's `modes` in `config/accounts.json` (`agentic`, `browser`,
`manual`) decide which execution blocks a ticket carries. `agentic` comes from config only, because
the server cannot see which account an agent's own connector may trade.

## More than one book per account

Accounts hold the data and books make claims on it, but a book may only cover accounts it is the
default book of (`internal/config`). Letting two books share an account needs a way for a book to
claim part of it, an account-level check that the claims do not overlap, and book-scoped views.

## Empty accounts cannot be snapshotted

`fetch_snapshot` fails for an account that has no orders yet: `internal/store/write.go` refuses to
record a position in an empty order log ("the log is empty; nothing to record a position in"), so a
brand-new account can never be snapshotted until its first order exists. Likely fix: let an empty log
have a position at sequence 0 and teach replay and the seal to accept it. Needs a regression test.

## Alpaca paper: order placement behind the broker interfaces

ARCHITECTURE.md lists an `Executor` capability for Alpaca paper only. Today paper orders can only be
staged through the agent tool `alpaca_stage_order` (`internal/alpaca`); there is no `Executor` in
`internal/broker`, and nothing in the wheel-book tools or the MCP server may ever place an order.

## Alpaca adapter gaps

Documented at the top of `internal/broker/alpaca/alpaca.go`: Alpaca reports no pending-buy
quantity (the `pending_buy_lag` check skips such positions), no minimum ticks, no realized P&L
(`ob reconcile` refuses), one aggregate fill per order, and the contract multiplier is assumed to be
100. Captured paper responses should replace the hand-written samples in
`internal/broker/alpaca/testdata` once there is a paper account with real activity.

## Ticket: verify what the account can do

The execution section of a ticket says "not verified: get_accounts is not stored in a snapshot", so
the option level and cash or margin that make an order possible are never checked
(`internal/report/execution.go`). Fix: have `fetch_snapshot` store `get_accounts` (it already calls
it) and have the ticket read it.

## Browser-entry checklist

The Robinhood entry checklist lists the web form's fields in a fixed order that has not been
checked against the live form (`internal/broker/robinhood/entry.go`). Nothing in the repo can check
a web page, so this needs someone with an account to compare.

## Rule backtest, in the repo

A backtest of a rule's closer target (for example the fraction of premium the closer buys back) that
can be re-run as the closed-leg count grows. It needs enough expired legs to say anything, so it waits
on data, and it should read only stored order history, like the ledger.

## Research data

Daily bars, option chains and rate series from free providers (for example OpenBB Platform run
locally as a REST server), stored as raw responses with a sha256 manifest, with every computation a
pure function of the saved files. First outputs: implied over realized volatility at the strike
being considered, a daily MACD histogram for a timing gate, rolling correlations, and rate series to
give a rates-driver rule numeric thresholds. **Research only**: never a source for ledger figures,
tickets or quotes, since spread and open interest are still confirmed at the broker.

## Housekeeping (good first issues)

- Output still says `wheel book` where the binary is now `ob` (`internal/render/wheelbook.go`).
- A few comments cite a planning document that is not in this repository ("item 6 step 6" in
  `internal/config`, "PLAN.md item 6, step 4" in `internal/broker/alpaca`, "item 6 step 5" in
  `internal/strategy`). Replace them with plain descriptions.
- A second source adapter (another broker) needs only `internal/broker/<source>` plus a pass of
  `internal/broker/conformance`; see ARCHITECTURE.md, "Adding a broker".
