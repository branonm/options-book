# Store on Cloud SQL (PostgreSQL)

**Status: decided 2026-11-19 by the owner; steps 1–5 done 2026-11-20.** The live server runs on
Cloud SQL for PostgreSQL; Firestore is no longer used by the deployment, though the code and the
Terraform defaults still support it (step 6 is the cleanup). No third-party database.

## Why

- **SQL is wanted going forward**: querying the agent's own records (the `run` documents
  today; later recommendations, risk checks and staged proposals) across check-ins.
  Firestore does this poorly and an object bucket can't do it at all.
- **The cost is accepted**: about $10–12 a month for the smallest instance (see Instance).
  Firestore was chosen originally because GCP has no always-free SQL database. Cloud Run,
  Secret Manager, the credential refresher and Artifact Registry stay within the free tier.

## What doesn't change

The store already sits behind a three-call, create-only interface (`internal/store/db.go`):
`Create` (or `ErrExists`), `Get`, and `List` sorted by id, over collections of `Doc`s whose
values are only `string`, `int64`, `bool` or `[]byte`. The Firestore backend
(`internal/store/firestore.go`) uses no transactions or batches. So this is **a new backend,
not a redesign**:

- every collection, document id and field stays as it is (`internal/store/schema.go`);
- the importer, `store.Open` (the store read back as the repo layout), `Export`,
  `ImportExport`, `Verify`, the server's write tools, and the shared checks
  (`history.Log.Prepare`, `snapshot.CheckAt`) are untouched;
- `Mem` stays the in-memory test backend, and `storetest.Sources` gains a `postgres`
  source alongside `files`, `memory` and `emulator`.

## Schema: one table now, typed views for querying

```sql
CREATE TABLE doc (
  coll       text        NOT NULL,   -- "snapshot", "raw_response/<sha256>/chunk", …
  id         text        NOT NULL,
  fields     jsonb       NOT NULL,   -- {"name": {"s": "…"} | {"i": "123"} | {"b": true} | {"x": "<base64>"}}
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (coll, id)
);
```

- **One generic table, not a table per collection.** It maps one-to-one onto the
  interface: `Create` is `INSERT … ON CONFLICT DO NOTHING` (zero rows inserted →
  `ErrExists`), `Get` is a primary-key lookup, and `List` is
  `WHERE coll = $1 ORDER BY id COLLATE "C"`. Byte order matches `Mem`'s and Firestore's
  sort; a test must prove that rather than assume it.
- **Every field keeps a type tag**, so an `int64` never becomes a float and `[]byte`
  round-trips exactly. Money stays a string, exactly as the broker sent it.
- **Raw responses stay split into 512 KiB chunks** (`raw_response/<sha256>/chunk`), even
  though Postgres has no 1 MiB document limit. Dropping the chunks would change the export
  and every hash-checked comparison, so it is a separate, later decision.
- **Querying comes from views** over `doc` (for example `run_v` or `order_version_v`, with
  typed columns pulled out of `fields`), added as questions come up. A view is derived, so
  adding or changing one never touches stored data. A real table per collection can follow
  if a query needs one.

**`fields` is `jsonb`** (owner, 2026-11-19), not `bytea` holding the exact encoding. Postgres
normalises key order, whitespace and escapes and keeps only the last of duplicate keys, but
nothing hashes the stored encoding: the hashes cover response bodies, which come back byte
for byte either way. The export → import → export check in step 4 proves the round trip.
One `jsonb` limit needs a guard: it can't hold a null character (`\u0000`) in a string, so
`checkDoc` refuses such a string for every backend, with an error that names the field.

## Append-only, enforced outside the server's code

Today a custom IAM role (`optionsBookInsertOnly`, `infra/gcp/main.tf`) grants
`datastore.entities.get`, `list` and `create`, with no update or delete. On Cloud SQL the
equivalent is three layers:

1. **The server's database user can only read and insert.** The server's service account
   signs in with Cloud SQL IAM database authentication, so there is no password anywhere.
   It is granted `SELECT, INSERT` on `doc` and nothing else: no `UPDATE`, `DELETE`,
   `TRUNCATE` or ownership.
2. **A trigger refuses `UPDATE` and `DELETE` for every user**, the owner included, so a
   mistake made as the owner fails too. Removing the trigger is a deliberate, visible act.
3. **The table is owned by a separate admin user** whose password only the owner holds. It
   is not in Secret Manager, and the server never sees it.

This is weaker than IAM in one respect: the table's owner *can* drop the trigger. Tampering
would still be caught, because `store.Open` and `Verify` refuse any hash, chain or seal
mismatch.

## Instance

| Setting | Value | Why |
|---|---|---|
| Edition, tier | Enterprise, `db-f1-micro` (shared core) | The smallest, about $8 a month in us-central1. Shared-core tiers aren't covered by the Cloud SQL SLA, which is acceptable for one user |
| Availability | Zonal | High availability doubles the price |
| Storage | 10 GB SSD, automatic increase on | The store is a few MB |
| Backups | Automated daily, 7 kept; deletion protection on | Point-in-time recovery off to start, since it adds log storage |
| Version | PostgreSQL 16 | Easy to run locally for tests |
| Connection | Public IP with no authorised networks, reached only through the Cloud SQL Go connector (`cloud.google.com/go/cloudsqlconn`) with IAM authentication | No VPC connector to pay for, and nothing can connect without `roles/cloudsql.client` |

Expected bill: **about $10–12 a month.** The $1 budget (`google_billing_budget.guard`) must
rise to about $15 in the same `terraform apply`, or it will alert every month.

## Driver

`github.com/jackc/pgx/v5` and `cloud.google.com/go/cloudsqlconn`. No ORM.

## Steps and pass conditions

Every step ships only if the existing gates still hold: the acceptance tests and both golden
files on every source, the diff and reconcile tests, `TestToolsMatchCLI` and
`TestCheckinThroughServer`.

| Step | Work | Pass condition |
|---|---|---|
| 1. Backend | `internal/store/postgres.go` implementing `DB`; the schema, grants and trigger as embedded SQL | The store's unit tests pass against a local PostgreSQL 16 (`POSTGRES_DSN` set): `ErrExists`, `List` order, every value type round-tripping, and `checkDoc` refusing a string with a null character |
| 2. Every source | `storetest.Sources` adds `postgres` when `POSTGRES_DSN` is set, like `emulator` with `FIRESTORE_EMULATOR_HOST` | Every test that loops over `storetest.Sources` passes on `files`, `memory`, `emulator` and `postgres` |
| 3. Infrastructure | Terraform: the instance, the IAM database user, `roles/cloudsql.client` for the server's service account, the raised budget | The owner runs `terraform apply`. A test connecting as the server's user shows `UPDATE` and `DELETE` refused |
| 4. Move the data | `ob db export` from Firestore → `ob db import` into Cloud SQL → `ob db verify` | The Cloud SQL export is **byte-identical** to the Firestore export |
| 5. Switch the server | `ob-mcp` takes a store flag (`firestore` or `postgres`); deploy | A live check-in done through the server matches the same check-in done with the file tools, byte for byte |
| 6. Retire Firestore | Remove the Firestore backend from the server, then the database and its IAM role | After a period the owner chooses; the last Firestore export is kept |

Steps 1 and 2 are offline and can start at any time. Steps 3–6 touch the live server, and
each waits for the owner's go-ahead.

## What changes elsewhere once decided

- `ARCHITECTURE.md`, "Storage and credentials: why it is built this way": the insert-only
  IAM role becomes the three layers above, and `internal/store`'s entry names the Postgres
  backend.
- `README.md`: `ob db` works against Cloud SQL as well as Firestore.
- `infra/gcp/main.tf`: `google_firestore_database.main` and the insert-only role go in
  step 6.
