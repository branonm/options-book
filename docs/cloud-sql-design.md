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

## Status

- **Steps 1 and 2: built and passing** against a local PostgreSQL 16, including the real-data tests
  in the private copy of the repository.
- **Steps 3 to 5: prepared, nothing run.** The code and Terraform exist and are tested offline
  (`terraform validate` passes), but no instance has been created, no data moved and no server
  switched. What was *not* tested: the Cloud SQL connector (`internal/store/cloudsql.go`) has never
  connected to a real instance, and `terraform plan` has not been run against the live project.
- **Step 6: not started.** It deletes the Firestore database and waits until the owner chooses.

What was added for 3 to 5:

- `store.NewCloudSQL`: the Cloud SQL Go connector with IAM database authentication.
- `ob db`: `--postgres DSN` and `--cloudsql INSTANCE/DATABASE/USER` for `import`, `export`, `verify`,
  `list` and `show`; `migrate [--server-role ROLE]`; and `check-append-only`. A DSN is never printed.
- `ob-mcp`: `STORE=cloudsql` with `CLOUDSQL_INSTANCE`, `CLOUDSQL_DATABASE` and `CLOUDSQL_USER`.
  Without `STORE` it uses Firestore, as today.
- `infra/gcp/cloudsql.tf` and variables: everything is off while `enable_cloudsql = false` (the
  default), so applying the Terraform unchanged creates nothing. `store_backend = "cloudsql"` points
  the server at it, and refuses to plan unless `enable_cloudsql` is also set.

## Runbook for steps 3 to 6

The owner runs every command that changes a live system.

**Before step 3: put the Terraform state somewhere safe.** Terraform's state holds every input
variable in plain text (the allowed account, for one), so it must never be committed, and a local
file ties it to one checkout. Move it to a private Cloud Storage bucket first:
1. Create the bucket outside Terraform (it must exist before Terraform can use it), with versioning
   on, uniform bucket-level access and public access prevention enforced.
2. `infra/gcp/main.tf` carries an empty `backend "gcs" {}` block, so the bucket name is never
   committed. Run `terraform init -migrate-state -backend-config="bucket=<bucket>"
   -backend-config="prefix=terraform/state"` from a directory that holds the existing state and
   this configuration. After that, every `terraform init` takes the same two `-backend-config`
   flags.
3. Run `terraform plan`. It must show **no changes**. Keep the old local state file until it does,
   then store it somewhere private and out of any repository.

**Step 3: infrastructure.**
1. Set `enable_cloudsql = true` in `terraform.tfvars`. Leave `store_backend` as `firestore`.
   The budget alert defaults to $20 a month. From Google's catalog prices on 2026-11-20 (zonal
   `db-f1-micro` $0.0105 an hour, 10 GiB standard storage at $0.17 per GiB-month, backups at $0.08
   per GiB-month), the instance costs about $10 a month, or up to about $17.50 if the public IPv4
   address is billed at $0.01 an hour; the first bill settles which. Re-check before changing the
   tier or the disk.
2. `terraform plan`. Expect: the Cloud SQL API, the instance, the database, two IAM database users,
   four IAM bindings and a changed budget, and **no change to the Cloud Run service**. Stop if the
   plan shows anything else.
3. `terraform apply`.
4. Create the schema as the owner, then grant the server: `ob db migrate --cloudsql
   <connection name>/<database>/<your Google account> --server-role <server user>`. Both values come
   from the Terraform outputs. **Unverified:** whether a Cloud SQL IAM user may create tables in the
   `public` schema of a new database. If the migration fails with a permission error on that schema,
   grant it once as the built-in `postgres` user, then run the migration again.
5. Show the grants hold: `ob db check-append-only --cloudsql <connection name>/<database>/<server
   user>`. It must report that all three statements are refused. Run it with the server's
   service-account credentials (impersonation), since it checks the connected user.

**Step 4: move the data.**
1. `ob db export --project <project> --to <empty dir>` from Firestore.
2. `ob db import --cloudsql <...>/<owner> --from-export <dir>`, which ends with a full verify.
3. `ob db export --cloudsql <...>/<owner> --to <second empty dir>` and compare the two directories
   with `diff -r`. They must be identical. Firestore is untouched throughout.

**Step 5: switch the server.**
1. Build and deploy an image containing this code **before** changing `store_backend`, so the service
   understands `STORE`.
2. Set `store_backend = "cloudsql"`, `terraform plan` (expect only the Cloud Run environment to
   change), then apply.
3. Check the new revision logs `store cloudsql=...`, an unauthenticated POST to `/mcp` still gets
   401, and `verify` and `list_snapshots` through the connector read the same data as before.
4. Do one live check-in through the server, as in the phase 4 gate.
5. **Rollback:** set `store_backend` back to `firestore` and apply. Nothing was removed from
   Firestore, so this restores the previous state, apart from anything written to Cloud SQL since.

**Step 6: retire Firestore.** Only after a period the owner chooses, and after keeping a last
Firestore export. Remove the Firestore database and its insert-only role in Terraform. This cannot be
undone.

## Effect on other docs (when decided)

- `ARCHITECTURE.md`, "Storage and credentials": the insert-only IAM role becomes the three layers
  above.
- `infra/gcp/main.tf`: `google_firestore_database.main` and the insert-only role go at step 6.
