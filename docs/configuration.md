# Configuration and commands

Configuration comes from environment variables only. Every secret `NAME` can instead be given as
`NAME_FILE`, a path to a file holding the value; setting both is an error.

This page lists what is implemented so far. It grows with each stage.

## Commands

| Command | What it does |
| --- | --- |
| `accquix-register migrate [up]` | Applies the embedded schema migrations, then grants the roles named by `REGISTER_DB_ROLE_*` their table rights |
| `accquix-register migrate status` | Lists the migrations and whether each is applied |
| `accquix-register migrate down-to N` | Rolls back to version N (0 = empty schema) |
| `accquix-register sync [--source rpo\|fs\|all]` | One sync pass (default `all`): the Register of Legal Entities (bootstrap, monthly reconcile, daily files) and the six Financial Administration lists. Safe to run often: a pass with nothing new costs a few requests (about 1–2 s) |
| `accquix-register sync --unblock --source rpo` or `--source fs:<list>` | After a validation block: restores the previous state. Run it once the cause is understood |
| `accquix-register suppress --subject <IČO> [--reason TEXT]` | Records an objection: keeps a subject out of name search. The subject is an IČO, or `r<RPO id>` for one record. It prints the objection's number and how many records it covers. It waits for a sync unit that is being applied |
| `accquix-register suppress --undo --id N` | Revokes one objection (or `--undo --subject <IČO>` when the subject has exactly one). It says whether another objection still hides the subject |
| `accquix-register suppress --list` | Prints every objection with its number, revoked ones included |
| `accquix-register suppress --export --output FILE [--overwrite]` | Writes the backup of the objections atomically. An existing file is replaced only with `--overwrite`, and never by an empty export. Keep the file private |
| `accquix-register suppress --restore FILE` | Puts a backup back (`-` reads standard input) and confirms the installation. It only adds |
| `accquix-register suppress --init-empty` | Confirms a new installation with no objections to restore. Until this or a restore, the API answers `503 reason=setup` |
| `accquix-register serve` | The read-only HTTP API (see below) |
| `accquix-register openapi` | Prints the OpenAPI 3.1 document; the committed copy is `api/openapi.yaml` |
| `accquix-register version` | Prints the version |

Exit codes: `0` success (also when another sync holds the lock), `1` failure (including a blocked
source), `2` a command-line mistake.

## Variables

| Variable | Default | Meaning |
| --- | --- | --- |
| `REGISTER_DATABASE_URL` (secret) | — | PostgreSQL connection string |
| `REGISTER_MIGRATE_DATABASE_URL` (secret) | `REGISTER_DATABASE_URL` | Used by `migrate`, for example with a DDL role |
| `REGISTER_SYNC_DATABASE_URL` (secret) | `REGISTER_DATABASE_URL` | Used by `sync`. **A direct connection**, not a transaction pooler: the sync holds an advisory lock and uses COPY |
| `REGISTER_OPERATOR_DATABASE_URL` (secret) | `REGISTER_SYNC_DATABASE_URL` | Used by `suppress`: the role that may change objections. A direct connection |
| `REGISTER_DB_ROLE_API`, `REGISTER_DB_ROLE_SYNC`, `REGISTER_DB_ROLE_OPERATOR` | — | Names of the database roles. `migrate` revokes everything they hold on the register's tables and grants exactly what each needs. Unset: nothing is granted (a single-role setup) |
| `REGISTER_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. Logs never contain names, search queries or identifier values |
| `REGISTER_RPO_BUCKET_URL` | the official MV SR bucket | Base URL of the RPO bulk files |
| `REGISTER_WORK_DIR` | `<temp>/accquix-register` | Downloads in progress. A bootstrap needs about 1 GB here; files are deleted once applied, and a partial download is resumed |
| `REGISTER_DOWNLOAD_CONCURRENCY` | `6` | Parallel snapshot part downloads |
| `REGISTER_RPO_PART_RECORDS` | `100000` | Records in every snapshot part except the last; a part that differs is rejected |
| `REGISTER_RPO_DAILY_MAX_RECORDS` | `20000` | A daily file with more records is rejected |
| `REGISTER_RPO_DATE_SKEW_DAYS` | `1` | A file whose `exportDate` is further than this from the date in its name is rejected. An `exportDate` after tomorrow is always rejected |
| `REGISTER_RPO_BOOTSTRAP_MIN_RECORDS` | `2200000` | A first snapshot with fewer records is not published (the register had about 2.28 M on 2026-11-07; review the default as it grows) |
| `REGISTER_RPO_SNAPSHOT_SETTLE` | `2h` | A snapshot is used only when its newest file has been in the bucket this long, so a set still being published is not taken for complete |
| `REGISTER_RPO_SNAPSHOT_MIN_RATIO` | `0.98` | A new snapshot smaller than this share of the previous one blocks absence marking |
| `REGISTER_ABSENCE_MAX_RATIO` | `0.005` | RPO: absence marking is blocked when more than this share of active records would be marked |
| `REGISTER_FS_BASE_URL` | `https://report.financnasprava.sk` | Base URL of the Financial Administration list exports |
| `REGISTER_FS_MAX_DROP` | per list | Overrides every FS list's threshold for a drop in rows and for rows marked absent. Defaults: 0.03 for `ds_dsrdp`, `ds_dphs`, `ds_iz_ran`; 0.20 for `ds_dphz`, `ds_dphv`, `ds_dpho` |
| `REGISTER_FS_SKIP_MIN_ROWS` | `false` | Every list has a row floor (`ds_dphs` 100 000, `ds_dsrdp` 500 000, `ds_iz_ran` 200 000, `ds_dphv` 10 000, `ds_dphz` 1 000, `ds_dpho` 300): a smaller version is rejected, also on the first load. `true` drops the floors, to accept a list that legitimately fell below one |

Every validation setting that differs from its default is an operator override: the sync logs
the list as a warning on every pass, so a loosened check is never in effect unnoticed.


### serve

| Variable | Default | Meaning |
| --- | --- | --- |
| `REGISTER_LISTEN` | `:8080` | Listen address |
| `REGISTER_API_KEYS` (secret) | — | `name:sha256hex,name2:sha256hex`: hashes of the keys, never the keys. Hash a key with `printf %s "$KEY" \| sha256sum`. Clients send `Authorization: Bearer <key>` |
| `REGISTER_ALLOW_ANONYMOUS` | `false` | Without keys, `serve` refuses to start unless this is `true`. Anonymous callers are rate-limited per IP; lookup of natural persons without an IČO by DIČ is then unavailable (D26) |
| `REGISTER_SEARCH_FUZZY` | `true` | Enables `mode=fuzzy`. When `false`, it answers `400 fuzzy_disabled` and `/v1/capabilities` says so |
| `REGISTER_RATE_PER_SECOND` / `REGISTER_RATE_BURST` | `20` / `40` | Token bucket per caller, per instance |
| `REGISTER_FUZZY_EVERY` | `5s` | One fuzzy search per caller per interval (burst 2) |
| `REGISTER_FUZZY_CONCURRENCY` | `1` | Fuzzy searches running at once, **per instance**: with several replicas, set a conservative value per replica (D18) |
| `REGISTER_FUZZY_TIMEOUT` | `2s` | A slower fuzzy search answers `503 timeout` |
| `REGISTER_QUERY_TIMEOUT` | `3s` | The same for default search and lookup |
| `REGISTER_SEARCH_WORD_STEP_MAX` / `REGISTER_SEARCH_WORD_CAP` | `1000` / `1000` | The adaptive word step (D17) |

Endpoints: `GET /v1/search`, `GET /v1/lookup/{id}`, `POST /v1/lookup:batch`, `GET /v1/sources`,
`GET /v1/capabilities` (key required); `/livez`, `/readyz`, `/openapi.json`, `/openapi.yaml`
(public). `/readyz` and the data endpoints answer `503 bootstrapping` until every source is first
published, and `503 setup` until the operator confirmed the objections of the installation. Errors are `application/problem+json` with a stable `code`. The access log records the
route, status, duration and key name only.

## How a sync pass behaves (RPO)

- **First run (bootstrap):** waits until the newest snapshot is complete: its manifest lists parts
  001..N without a hole, every one is in the bucket, the bucket holds no part the manifest does
  not list, and every file has been there for `REGISTER_RPO_SNAPSHOT_SETTLE`. A snapshot with two
  or more parts fewer than the previous one in the bucket is rejected. It downloads the parts in
  parallel, applies them (every part but the last must hold exactly the part size), applies the
  daily files after the snapshot, checks that the manifest did not change meanwhile and that the
  total reaches `REGISTER_RPO_BOOTSTRAP_MIN_RECORDS`, builds the search layer and only then
  publishes (D22). If a newer snapshot appears before publication, the bootstrap starts over
  with it: rows of the abandoned snapshot are never published.
- **File names:** a name with an impossible date (`init_2026-02-31_…`) is ignored and reported in
  the log; it is never taken for a snapshot.
- **Later runs:** applies the next daily file, one day at a time. A missing day is never skipped:
  the state becomes `gap_wait` until the file arrives or a newer snapshot heals the gap.
- **Monthly:** a complete snapshot newer than the last one is reconciled: changed records are
  merged under the order guard, and records it no longer contains are marked absent (within the
  threshold).
- **Validation (D23):** a file that breaks a threshold is not applied, the previous data stays, and
  the source becomes `blocked` with a diagnostic until `--unblock`. This includes the file's
  `exportDate`: a date in the future, or far from the date in the file name, is rejected before
  anything is staged. Updates are ordered by the date in the file name (the order they are applied
  in); the `exportDate` is stored as the data's as-of date.
- **Crash safety:** every file is one transaction with a checkpoint; a killed pass resumes where it
  stopped.

## How a sync pass behaves (FS lists)

- Each list is independent: a conditional GET with the stored ETag (`304` → nothing to do); the
  same content under a new ETag only updates the ETag; a new version is one unit.
- **Validation (D23):** the list date must not be older than the applied one and not after
  tomorrow; a large list must reach its row floor; every item must parse
  (0 errors); the row count may drop, and rows may disappear, by at most the list's threshold.
  A list that fails is `blocked` alone; the others continue.
- Duplicate keys keep the row with the lowest content hash; their number is in the unit's detail.
- Rows that disappear get `absent_since`: an observation, never a legal date.
- **To accept a list that legitimately shrank:** `sync --unblock --source fs:<list>`, then one run
  with `REGISTER_FS_MAX_DROP` set high enough.
