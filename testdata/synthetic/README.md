# Synthetic repo root

Everything here is fictional, written by `go run ./tools/synthfixture` in the shape of Robinhood's
MCP responses. No real account, holding, price or date of a trade is in it. The order log and
manifests are made by the repo's own commands (history add, snapshot, expirations), so they
are sealed the way real ones are.

Two accounts, **roth** (••0001) and **trad** (••0002); campaign start 2026-01-05. Symbols AAA, BBB, CCC.

## Hand-computed figures (what the tests pin)

The rules are the synthetic ones in books/ira-wheel/rules.json (hand-written example values,
nothing from a real book). Rule 10: the closer is 50% of the fill, rounded up to the tick (a penny under $3).

| Account | Event | Figure |
|---|---|---|
| roth | AAA ×3 $55c exp 1/23 sold $1.00 (credit $300), closer $0.50 filled 1/16 (debit $150) | net **+$150**, realized |
| roth | AAA ×3 $55c exp 3/20 sold $1.50 (credit $450), closer $0.75 | open |
| roth | BBB ×1 $85c exp 2/20 sold $2.00 (credit $200) | open at 2/06 |
| trad | CCC ×2 $44c exp 1/30 sold $0.50 (credit $100), expired worthless (CCC closed 41.20) | net **+$100**, realized |
| trad | CCC ×2 $44c exp 2/20 sold $0.80 (credit $160), closer $0.40 | open |

**Snapshot 2026-02-06** (after close). Roth realized **150**, open credit **650** (450 + 200);
trad realized **100**, open credit **160**. Blocks: AAA 300 sh premium 600 (150 + 450), breakeven
50 − 600/300 = **48.00**; BBB 100 sh premium 200, breakeven 80 − 2 = **78.00**; CCC 200 sh premium 260
(100 + 160), breakeven 40 − 260/200 = **38.70**. One defect: the BBB closer is $1.10, Rule 10 says $1.00.

**Snapshot 2026-02-13.** The $1.10 closer was cancelled and replaced by $1.00, which filled 2/10
(debit $100). Roth realized **250**, open credit **450**; BBB premium 100, breakeven 80 − 1.00 = **79.00**.
Trad unchanged. No defects; one alert, `uncovered_shares` on BBB (its call was bought back and not re-sold).
Reconcile 2026-02-13 agrees to $0.00 (roth 250, trad 100).
