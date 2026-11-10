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
- handle objections: `accquix-register suppress --subject <IČO> --reason <ticket>` keeps a
  subject out of name search (exact lookup still works); `--undo` reverses it;
- the register stores only what identifies a business: no residence when it differs from the place
  of business, no birth data, no statutory bodies or representatives. Logs contain no queries and
  no identifiers.

## Requirements (measured on 2026-11-10 data)

| Resource | Minimum | Comfortable |
| --- | --- | --- |
| PostgreSQL disk | 5 GB (about 2.3 GB of data, about 0.75 GB WAL during loads, headroom) | 10 GB |
| PostgreSQL RAM | 1 GB | 2–4 GB |
| PostgreSQL CPU | 1/8 vCPU: about 30 default searches/s when saturated | 1/4 vCPU or more: 75–140/s. Fuzzy search is the CPU-heavy part |
| Register container | 256 MB RAM (the sync peaks at about 30 MB) | 512 MB |
| Work volume for `sync` | 1 GB during the first run | — |
| Outbound HTTPS | the RPO bucket (Oracle Cloud, Frankfurt) and `report.financnasprava.sk` | — |
| First run | about 10–20 minutes, mostly downloading about 1 GB | — |

Only the `pg_trgm` extension is needed. It works on plain PostgreSQL, Neon, PlanetScale Postgres,
Supabase and RDS.

## Quick start with docker compose

```sh
git clone https://github.com/accquix/register && cd register/deploy/compose
./make-secrets.sh            # random database password and one API key (printed once)
docker compose up -d         # postgres → migrate → serve + sync (hourly)
docker compose logs -f sync  # the first run downloads and publishes the data
curl -s localhost:8080/readyz
curl -s -H "Authorization: Bearer <key>" "localhost:8080/v1/search?q=tatra%20banka"
```

`make-secrets.sh` keeps `secrets/` private (mode 0700) and makes the files inside readable (0644):
compose mounts them as they are, and the containers do not run as your user. If you write the
files yourself, do the same, or the register fails with `permission denied` on
`/run/secrets/...`.

The API answers `503 bootstrapping` until the first sync has published every source. The API is
published on `127.0.0.1:8080`; set `REGISTER_PUBLISH=0.0.0.0:8080` only behind your own TLS
proxy.

## Other platforms

The image runs anywhere that offers a long-lived HTTP container and a scheduled job:

- `serve`: the HTTP API (port 8080), stateless; scale it horizontally.
- `sync`: either `sync --every 1h` as a long-running container, or a plain `sync` started by your
  scheduler (cron, Kubernetes CronJob, ECS scheduled task, Cloud Run job…) every 1–2 hours. Only one
  sync runs at a time (advisory lock); an overlapping start exits at once.
- `migrate`: run it before starting a new version.

## Database roles

Separate roles keep the API read-only:

```sql
CREATE ROLE register_migrate LOGIN PASSWORD '...';   -- owns the schema, runs migrate
CREATE ROLE register_sync    LOGIN PASSWORD '...';
CREATE ROLE register_api     LOGIN PASSWORD '...';
-- after `migrate` as register_migrate:
GRANT USAGE ON SCHEMA public TO register_sync, register_api;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON ALL TABLES IN SCHEMA public TO register_sync;
GRANT USAGE ON ALL SEQUENCES IN SCHEMA public TO register_sync;
GRANT SELECT ON ALL TABLES IN SCHEMA public TO register_api;
-- tables added by later migrations get the same rights:
ALTER DEFAULT PRIVILEGES FOR ROLE register_migrate IN SCHEMA public
  GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLES TO register_sync;
ALTER DEFAULT PRIVILEGES FOR ROLE register_migrate IN SCHEMA public GRANT SELECT ON TABLES TO register_api;
```

Then set `REGISTER_MIGRATE_DATABASE_URL`, `REGISTER_SYNC_DATABASE_URL` and `REGISTER_DATABASE_URL`
(the API) to the three roles. The sync creates temporary tables, so it also needs
`TEMPORARY` on the database (granted to `PUBLIC` by default).

**Connections:** the sync needs a **direct** connection (advisory lock, COPY); the API can go
through a transaction pooler (PgBouncer). Use `sslmode=verify-full` to managed databases.

**PlanetScale Postgres:** `pg_strict` is compatible (the register never runs `UPDATE`/`DELETE`
without `WHERE`); use port 5432 for the sync and 6432 (PgBouncer) for the API.

## Backups

Not needed. The database can always be rebuilt from the official sources: drop it, run `migrate`
and `sync`. Section versions are content hashes, so a rebuild gives your consumers the same
versions.

## Monitoring

- `GET /readyz`: 503 until every source is first published, or when the database is unreachable.
- `GET /v1/sources`: per source, the state (`daily`, `gap_wait`, `reconcile`, `blocked`…), the
  as-of date and `stale`. Alert when:
  - any source is `blocked` (validation stopped an update; see the diagnostic, then
    `sync --unblock --source <source>`; docs/configuration.md);
  - `rpo` is `gap_wait` for more than a day, or any source is `stale`.
- Freshness thresholds: RPO 3 days, the FS lists 2 days, the reliability index 14 days.

## Upgrades

Pull the new image, run `migrate`, then restart `serve` and `sync`. Migrations are forward-only in
practice; `migrate down-to` exists for development.

## Configuration

Every variable and command is listed in [configuration.md](configuration.md).
