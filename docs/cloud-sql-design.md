# Store on Cloud SQL (PostgreSQL)

**Status: proposal.** The direction is to stay on GCP and add a PostgreSQL backend (Cloud SQL) in
place of Firestore. Nothing below is built yet, and nothing here changes the live server. This
supersedes the earlier choice of Firestore, which was made because GCP has no always-free SQL
database. No third-party database is wanted.

## Why

- **SQL is wanted going forward**, to query the agent's own records (runs, and later
  recommendations, risk checks and staged proposals) across check-ins. Firestore can't do that
  well, and a bucket can't do it at all.
- **The cost is accepted**: roughly the price of the smallest Cloud SQL instance (see
  "Instance"). Cloud Run, Secret Manager, the refresher job and Artifact Registry stay inside
  the free tier.

## What doesn't change

The store is already behind a three-call, create-only interface (`internal/store/db.go`):
`Create` (or `ErrExists`), `Get`, and `List` sorted by id, over collections of `Doc`s whose values
are only `string`, `int64`, `bool` or `[]byte`. The Firestore backend (`firestore.go`) uses no
transactions or batches. So this is **a new backend, not a redesign**:

- every collection, document id and field stays as it is (`schema.go`);
- the importer, `Open` (the store read back as the repo layout), `Export`, `ImportExport`,
  `Verify`, the write tools and every check in `history.Log.Prepare` and `snapshot.CheckAt` are
  untouched;
- `Mem` stays the test backend, and `storetest.Sources` gains a `postgres` source.

## Schema: one table now, typed views for querying

```sql
CREATE TABLE doc (
  coll       text        NOT NULL,   -- "snapshot", "raw_response/<sha256>/chunk", ...
  id         text        NOT NULL,
  fields     jsonb       NOT NULL,   -- {"name": {"s": "..."} | {"i": "123"} | {"b": true} | {"x": "<base64>"}}
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (coll, id)
);
```

- **One generic table, not a table per collection.** It is exactly the interface: `Create` is
  `INSERT ... ON CONFLICT DO NOTHING` (zero rows affected means `ErrExists`), `Get` is a
  primary-key lookup, and `List` is `WHERE coll = $1 ORDER BY id COLLATE "C"`. The `"C"` collation
  is byte order, to match Firestore's and `Mem`'s sort. That must be tested, not assumed.
- **Fields keep their type tag**, so an `int64` never becomes a float and `[]byte` round-trips
  exactly. Money stays a string, as today.
- **Chunks stay** (`raw_response/<sha>/chunk`, 512 KiB) even though Postgres has no 1 MiB limit.
  Removing them would change the export and every hash-verified gate. That is a separate, later
  decision.
- **Querying comes from views** over `doc` (for example `run_v` and `order_version_v`, with typed
  columns pulled out of `fields`), added as questions come up. A view is derived, so adding or
  changing one never touches stored data. A real table per collection can come later if a query
  needs it.

**Open:** whether `fields` should be `jsonb` (queryable, but Postgres normalises key order and
whitespace) or `bytea` of the exact encoding. Nothing hashes the stored encoding today (the
hashes are over bodies, which come back byte for byte either way), so `jsonb` looks safe. The
export, import, export gate would prove it.

**Values Postgres cannot hold.** A `text` or `jsonb` value cannot contain a NUL (`\x00`) and must
be valid UTF-8. `checkDoc` and `checkID` gain a guard so such a value is refused with a clear
error before it reaches the database.

## Append-only, enforced outside the server's code

On Firestore, append-only is a custom IAM role with `datastore.entities.get`, `list` and `create`
and no update or delete. On Cloud SQL the equivalent is three layers:

1. **The server's database user can only read and insert.** The server's service account signs in
   with Cloud SQL IAM database authentication (no password anywhere) as a user granted `SELECT`
   and `INSERT` on `doc` and nothing else: no `UPDATE`, `DELETE`, `TRUNCATE` or ownership.
2. **A trigger refuses `UPDATE`, `DELETE` and `TRUNCATE` for everyone**, the owner included, so a
   mistake made as the owner fails too. Removing it is a deliberate, visible act.
3. **The table is owned by a separate admin user** whose password the owner holds. It is not in
   Secret Manager and the server never sees it.

This is weaker than IAM in one way: the owner can drop the trigger. Tampering would still be
caught, because `Open` and `Verify` refuse any hash, chain or seal mismatch.

## Instance

These are unverified starting points. Check Google's current pricing before relying on any cost.

| Setting | Value | Why |
|---|---|---|
| Edition, tier | Enterprise, `db-f1-micro` (shared core) | The smallest. It is not covered by the Cloud SQL SLA, which is acceptable for one user |
| Availability | Zonal | High availability roughly doubles the price |
| Storage | 10 GB SSD, auto-increase on | The whole store is a few MB |
| Backups | Automated daily, 7 kept; deletion protection on | Point-in-time recovery off to start, since it adds log storage cost |
| Version | PostgreSQL 16 | Matches the version used for tests |
| Connection | Public IP with no authorised networks, reached only through the Cloud SQL Go connector (`cloud.google.com/go/cloudsqlconn`) with IAM auth | No VPC or connector to pay for; nothing can reach it without `roles/cloudsql.client` |

The `$1` budget guard (`google_billing_budget.guard` in `infra/gcp/main.tf`) must be raised in the
same `terraform apply`, or it will alert every month.

## Driver

`github.com/jackc/pgx/v5`, plus `cloudsqlconn` for the Cloud SQL connection (a later step). No ORM.

## Steps and gates

Each step ships only if every standing gate still holds: both goldens, `reconcile` agreeing to
$0.00, the check-in acceptance tests, the tool-versus-CLI match and `TestCheckinThroughServer`.

| Step | Work | Pass condition |
|---|---|---|
| 1. Backend | `internal/store/postgres.go` implementing `DB`; schema, grants and trigger as embedded SQL; the NUL and UTF-8 guard | The store's tests pass against a local PostgreSQL 16 (`POSTGRES_DSN` set), including `ErrExists`, `List` order, every value type round-tripping, and `UPDATE`, `DELETE` and `TRUNCATE` refused |
| 2. Every source | `storetest.Sources` adds `postgres` when `POSTGRES_DSN` is set; `TestCheckinThroughServer` gains a postgres store | Every acceptance test, both goldens, the diff, reconcile and `TestCheckinThroughServer` pass on files, memory, the emulator (when set) and postgres |
| 3. Infrastructure | Terraform: the instance, the IAM database user, `roles/cloudsql.client` for the server, the raised budget; grants and trigger applied by the migration | The owner runs `terraform apply`. A test connects as the server's user and shows `UPDATE` and `DELETE` refused |
| 4. Move the data | `ob db export` from Firestore, then `ob db import` into Cloud SQL, then `ob db verify` | The Cloud SQL export is **byte-identical** to the Firestore export |
| 5. Switch the server | The server takes `--store=postgres`; deploy | A live check-in through the server matches the file run (the phase 4 gate, repeated) |
| 6. Retire Firestore | Stop using the Firestore backend in the server, then remove the database and its IAM role | After a period the owner chooses; the last Firestore export is kept |

Steps 1 and 2 are offline and touch no live system. Steps 3 to 6 touch the live server, and each
waits for the owner's go-ahead.

## Effect on other docs (when decided)

- `ARCHITECTURE.md`, "Storage and credentials": the insert-only IAM role becomes the three layers
  above.
- `infra/gcp/main.tf`: `google_firestore_database.main` and the insert-only role go at step 6.
