# Self-hosting

Accquix Register runs as one container image against any PostgreSQL 16–18. Your instance
downloads the data from the official sources itself; this repository contains no data.

## You are the controller

The registers contain personal data: about 80 % of RPO records are natural persons, mostly sole
traders. Their business name and place of business are personal data even though the law
publishes them. When you run an instance, **you** are the controller of that copy:

- decide and document your purpose and legal basis before offering name search to anyone;
- keep the API behind keys (the default); anonymous access needs `REGISTER_ALLOW_ANONYMOUS=true`
  and then disables the DIČ lookup of natural persons without an IČO;
- handle objections: `accquix-register suppress --subject <IČO> --reason <ticket>` records an
  objection and keeps the subject out of name search (exact lookup still works);
  `suppress --undo --id <N>` revokes that one objection. The objections are the one thing the
  official sources cannot give back: **back them up** (see [Objections](#objections-your-own-data));
- the register stores only what identifies a business: no residence when it differs from the place
  of business, no birth data, no statutory bodies or representatives. Logs contain no queries and
  no identifiers.

## Requirements (measured on 2026-11-10 data)

| Resource | Minimum | Comfortable |
| --- | --- | --- |
| PostgreSQL disk | 5 GB (about 2.3 GB of data, about 0.75 GB WAL during loads, headroom) | 10 GB |
| PostgreSQL RAM | 1 GB | 2–4 GB |
| PostgreSQL CPU | 1/8 vCPU: about 85 default searches/s at the limit, 60/s sustained at full CPU; plan for up to about 30/s | 1/4 vCPU or more: about 100–110/s at the limit; plan for up to about 50/s ([managed-postgres-results.md](managed-postgres-results.md)) |
| Register container | 256 MB RAM (the sync peaks at about 30 MB) | 512 MB |
| Work volume for `sync` | 1 GB during the first run | — |
| Outbound HTTPS | the RPO bucket (Oracle Cloud, Frankfurt) and `report.financnasprava.sk` | — |
| First run | about 10–20 minutes, mostly downloading about 1 GB | — |

Only the `pg_trgm` extension is needed. It works on plain PostgreSQL, Neon, PlanetScale Postgres,
Supabase and RDS.

## Quick start with docker compose

```sh
git clone https://github.com/accquix/register && cd register/deploy/compose
./make-secrets.sh            # random database passwords and one API key (printed once)
docker compose up -d         # postgres → migrate → serve + sync (hourly)
docker compose run --rm operator suppress --init-empty   # a new installation: no objections to restore
docker compose logs -f sync  # the first run downloads and publishes the data
curl -s localhost:8080/readyz
curl -s -H "Authorization: Bearer <key>" "localhost:8080/v1/search?q=tatra%20banka"
```

`make-secrets.sh` keeps `secrets/` private (mode 0700) and makes the files inside readable (0644):
compose mounts them as they are, and the containers do not run as your user. If you write the
files yourself, do the same, or the register fails with `permission denied` on
`/run/secrets/...`.

Compose creates four database roles when PostgreSQL first starts (`initdb/10-roles.sh`), and each
process gets only its own: `migrate` owns the schema, `sync` writes the mirror, `operator` changes
the objections, `serve` can only read the published data. No register process connects as the
superuser.

The API is not ready (`503`) until two things are true:

- every source is published by the first sync (`reason=bootstrapping`);
- you confirmed the objections of this installation (`reason=setup`): `suppress --init-empty` on a
  new installation, `suppress --restore` on a rebuilt one. An empty database cannot tell which of
  the two it is, so the register does not guess.

The API is published on `127.0.0.1:8080`; set `REGISTER_PUBLISH=0.0.0.0:8080` only behind your own
TLS proxy.

## Other platforms

The image runs anywhere that offers a long-lived HTTP container and a scheduled job:

- `serve`: the HTTP API (port 8080), stateless; scale it horizontally.
- `sync`: either `sync --every 1h` as a long-running container, or a plain `sync` started by your
  scheduler (cron, Kubernetes CronJob, ECS scheduled task, Cloud Run job…) every 1–2 hours. Only one
  sync runs at a time (advisory lock); an overlapping start exits at once.
- `migrate`: run it before starting a new version.

## Database roles

Separate roles keep each process to its job. The compose deployment creates them for you; on your
own PostgreSQL, create them as an administrator **before** `migrate`:

```sql
CREATE ROLE register_migrate  LOGIN PASSWORD '...';   -- owns the schema, runs migrate
CREATE ROLE register_sync     LOGIN PASSWORD '...';   -- sync
CREATE ROLE register_operator LOGIN PASSWORD '...';   -- suppress (objections)
CREATE ROLE register_api      LOGIN PASSWORD '...';   -- serve
ALTER DATABASE register OWNER TO register_migrate;    -- or: GRANT CREATE ON DATABASE and ON SCHEMA public
GRANT CONNECT ON DATABASE register TO register_sync, register_operator, register_api;
GRANT TEMPORARY ON DATABASE register TO register_sync, register_operator;
ALTER ROLE register_api SET default_transaction_read_only = on;
ALTER ROLE register_api SET statement_timeout = '10s';
```

Then run `migrate` with the role names, and it grants every role exactly the table rights it
needs, after every migration (the matrix is in `internal/migrate/grants.go`):

```sh
REGISTER_DB_ROLE_API=register_api REGISTER_DB_ROLE_SYNC=register_sync \
REGISTER_DB_ROLE_OPERATOR=register_operator accquix-register migrate
```

| Role | May | May not |
| --- | --- | --- |
| API | read the mirror, the search layer and *which* keys are suppressed | read the reasons or history of objections, the sync journal or staging; write anything |
| sync | write the mirror, the search layer and its journal; read the objections | add, change or revoke an objection; confirm the installation |
| operator | add and revoke objections and rebuild the search rows they affect; confirm the installation | write register data; delete an objection (a revoked one stays as its record) |

Set `REGISTER_MIGRATE_DATABASE_URL`, `REGISTER_SYNC_DATABASE_URL`,
`REGISTER_OPERATOR_DATABASE_URL` and `REGISTER_DATABASE_URL` (the API) to the four roles. Never
give `serve` a role that can write. With a single role for everything, leave the
`REGISTER_DB_ROLE_*` variables unset: `migrate` then grants nothing.

`migrate` checks what it granted. It refuses a role that owns the tables, is a superuser, or
still holds a right it must not have, and it says which. A role may be a member of another role,
but it inherits that role's rights: a role created in a provider's console is often a member of an
administrative role (on Neon, of `neon_superuser`) and is then refused. Create the three roles
with SQL, as above. `migrate` changes rights only on the register's own tables, so other tables in
the same schema are left alone.

**Supabase:** tables in the `public` schema are exposed through the project's Data API to the
`anon` and `authenticated` roles by default. Keep the register in a project of its own, or switch
the Data API off for it, and check that those roles hold no rights on the register's tables,
above all on `objection`.

PostgreSQL 16, 17 and 18 are tested. On 17 and 18 `migrate` also grants `MAINTAIN` to the sync
role, so that it can `ANALYZE` the search tables after a rebuild; on 16 autovacuum does that.

**Connections:** the sync and the operator need a **direct** connection (advisory locks, COPY,
temporary tables); the API can go through a transaction pooler (PgBouncer). Use
`sslmode=verify-full` to managed databases.

**API connection pool:** `serve` opens at most 10 connections unless the URL sets `pool_max_conns`
(D40), and runs at most that many data requests at once; the excess gets `503 overloaded` after a
short wait (D39). When the database is not next to the API, the pool can set the limit: at 20 ms
per round trip each connection carries about 7 searches a second. Set the size in the URL, for
example `?sslmode=verify-full&pool_max_conns=12` in `REGISTER_DATABASE_URL`, within the database's
connection limit.

**PlanetScale Postgres:** tested on 2026-11-11 with the real data
([managed-postgres-results.md](managed-postgres-results.md)). `pg_strict` with both settings `on`
is compatible (the register never runs `UPDATE`/`DELETE` without `WHERE`); use port 5432 for the
sync and the operator, and 6432 (PgBouncer) for the API. The small sizes allow 25 connections.

## Objections: your own data

The register data needs no backup: it can always be rebuilt from the official sources, and section
versions are content hashes, so a rebuild gives your consumers the same versions.

**The objections are different.** They are your own data. If they are lost, every person who
objected silently returns to name search, and no source can restore them. Dropping the database,
`docker compose down -v` and `migrate down-to 0` all delete them.

**How an objection works**

An objection is the person's request. A *link* says that the objection covers one RPO record, why,
and since when. The two are kept apart, so a mistake in a link is corrected without touching the
request.

- `suppress --subject <IČO>` (or `r<RPO id>` for one record) records an objection with its own
  number and links it to the records that carry that IČO now.
- The sync links every record it later sees carrying the IČO, and logs a warning with the
  objection's number and the file. A linked record stays hidden when it loses or changes its IČO:
  no record leaves an objection unless you decide so.
- A link covers that one record. It never extends to other records that share the record's new
  IČO, so an objection does not spread to another subject.
- **The register cannot tell a source mistake from a real change, and it errs towards hiding.** If
  a file wrongly gives another subject's record the objected IČO, that one record is linked and
  stays out of name search after the source corrects itself. This is a known limitation, not a
  silent one: the link shows in the log and in `suppress --list --id <N>` with the file that
  caused it.
- `suppress --release --id <N> --record <R> --reason <ticket>` excludes that one record from the
  objection. The record must be linked to it (`suppress --list --id <N>`); any other record id is
  refused. The exclusion stays on record, so the next sync does not link the record again, also
  when a later file repeats the mistake. `suppress --unrelease --id <N> --record <R>` withdraws
  it. The objection and its other links are untouched.
- `suppress --undo --id <N>` revokes one objection and nothing else, and says how many of its
  records are still hidden by other objections. A revoked objection stays in the list as the
  record of the undo.
- `suppress --list` prints every objection; `--list --id <N>` prints its links: the record, the
  basis (`at_objection` by your command, `carried_ico` by the sync, `restored`, `migrated`), the
  file, and whether it is released.

Review the sync's link warnings when they appear: each is a record the register hid on its own
authority.

**Backup**

```sh
accquix-register suppress --export --output objections.tsv              # a new file
accquix-register suppress --export --output objections.tsv --overwrite  # replace the previous one
# compose: docker compose run --rm -v "$PWD/backup:/backup" operator suppress --export --output /backup/objections.tsv
```

- Export after every change, and on a schedule. The file is written atomically and is never
  overwritten without `--overwrite`. With it, the existing file is replaced only when it is a valid
  backup and every objection in it is also in the new one: a backup can only grow, so an empty or
  rebuilt database can never replace your only good copy. An unconfirmed installation cannot
  export at all. Do not redirect `--list` into
  a file: the shell truncates the file before the command runs.
- The file lists the people who objected. Keep it encrypted (for example with `age` or `gpg`),
  with restricted access, away from the database.
- Its last line holds the row count and a checksum, so a restore refuses a truncated or damaged
  file. This is integrity, **not** authenticity: someone who can change the file can recompute the
  checksum. Access control and encryption are what protect it.

**Rebuilding the database**

1. `migrate`;
2. `accquix-register suppress --restore objections.tsv` (it only adds: an objection or a link
   that the database already has is kept exactly as it is, so a restore never revokes an
   objection and never releases a record);
3. `sync` and `serve`.

The register enforces this: until the restore (or, on a new installation, `suppress --init-empty`)
the API answers `503 reason=setup` and `/readyz` is not ready. In compose:
`docker compose up -d`, then
`docker compose run --rm -T operator suppress --restore - < objections.tsv`.

If you lost both the database and every copy of the backup, the register cannot recover the
objections. `suppress --init-empty` then starts an empty list; you must rebuild it from your own
records of the requests (tickets, mail).

## Monitoring

- `GET /readyz`: 503 until every source is first published and the objections are confirmed, or
  when the database is unreachable.
- `GET /v1/sources`: per source, the state (`daily`, `gap_wait`, `reconcile`, `blocked`…), the
  as-of date and `stale`. Alert when:
  - any source is `blocked` (validation stopped an update; see the diagnostic, then
    `sync --unblock --source <source>`; docs/configuration.md);
  - `rpo` is `gap_wait` for more than a day, or any source is `stale`.
- Freshness thresholds: RPO 3 days, the FS lists 2 days, the reliability index 14 days.

## Upgrades

Pull the new image, run `migrate`, then restart `serve` and `sync`. Migrations are forward-only in
practice; `migrate down-to` exists for development, and `down-to 0` deletes the objections.

### Upgrading from the single-role layout

A compose deployment made before the roles were separated used one superuser, `register`, for
every process. To move it, keeping the data, the objections and the API keys:

```sh
docker compose down            # keeps the pgdata volume
./make-secrets.sh --upgrade    # adds the new secret files; sets the old superuser URL aside
./upgrade-roles.sh             # creates the roles and hands the database to register_migrate
docker compose up -d
```

`migrate` then applies the new migrations and grants the table rights. Existing objections are
carried over, and a database that already published data stays confirmed. The old superuser
remains in that data directory for administration; no register process uses it. Without
`--upgrade`, `make-secrets.sh` stops and says so, and the new compose file does not start with the
old secrets: it fails closed.

On your own PostgreSQL the same steps are: create the roles (above), make `register_migrate` the
owner of the database **and of every table, sequence and view** in `public`
(`ALTER TABLE … OWNER TO register_migrate`; `migrate` must own what it alters), then run
`migrate` with the `REGISTER_DB_ROLE_*` variables.

## Configuration

Every variable and command is listed in [configuration.md](configuration.md).
