# Configuration and commands

Configuration comes from environment variables only. Every secret `NAME` can instead be given as
`NAME_FILE`, a path to a file holding the value; setting both is an error.

This page lists what is implemented so far. It grows with each stage.

## Commands

| Command | What it does |
| --- | --- |
| `accquix-register migrate [up]` | Applies the embedded schema migrations |
| `accquix-register migrate status` | Lists the migrations and whether each is applied |
| `accquix-register migrate down-to N` | Rolls back to version N (0 = empty schema) |
| `accquix-register sync [--source rpo]` | One sync pass of the Register of Legal Entities: bootstrap, monthly reconcile and daily files, as far as the bucket allows. Safe to run often: a pass with nothing new costs a few requests |
| `accquix-register sync --source rpo --unblock` | After a validation block: restores the previous state. Run it once the cause is understood |
| `accquix-register version` | Prints the version |

Exit codes: `0` success (also when another sync holds the lock), `1` failure (including a blocked
source), `2` a command-line mistake.

## Variables

| Variable | Default | Meaning |
| --- | --- | --- |
| `REGISTER_DATABASE_URL` (secret) | — | PostgreSQL connection string |
| `REGISTER_MIGRATE_DATABASE_URL` (secret) | `REGISTER_DATABASE_URL` | Used by `migrate`, for example with a DDL role |
| `REGISTER_SYNC_DATABASE_URL` (secret) | `REGISTER_DATABASE_URL` | Used by `sync`. **A direct connection**, not a transaction pooler: the sync holds an advisory lock and uses COPY |
| `REGISTER_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. Logs never contain names, search queries or identifier values |
| `REGISTER_RPO_BUCKET_URL` | the official MV SR bucket | Base URL of the RPO bulk files |
| `REGISTER_WORK_DIR` | `<temp>/accquix-register` | Downloads in progress. A bootstrap needs about 1 GB here; files are deleted once applied, and a partial download is resumed |
| `REGISTER_DOWNLOAD_CONCURRENCY` | `6` | Parallel snapshot part downloads |
| `REGISTER_RPO_PART_RECORDS` | `100000` | Records in every snapshot part except the last; a part that differs is rejected |
| `REGISTER_RPO_DAILY_MAX_RECORDS` | `20000` | A daily file with more records is rejected |
| `REGISTER_RPO_SNAPSHOT_MIN_RATIO` | `0.98` | A new snapshot smaller than this share of the previous one blocks absence marking |
| `REGISTER_ABSENCE_MAX_RATIO` | `0.005` | Absence marking is blocked when more than this share of active records would be marked |

## How a sync pass behaves (RPO)

- **First run (bootstrap):** waits until the newest snapshot is complete (its manifest and every
  listed part are in the bucket), downloads the parts in parallel, applies them, applies the daily
  files after the snapshot, builds the search layer and only then publishes (D22).
- **Later runs:** applies the next daily file, one day at a time. A missing day is never skipped:
  the state becomes `gap_wait` until the file arrives or a newer snapshot heals the gap.
- **Monthly:** a complete snapshot newer than the last one is reconciled: changed records are
  merged under the order guard, and records it no longer contains are marked absent (within the
  threshold).
- **Validation (D23):** a file that breaks a threshold is not applied, the previous data stays, and
  the source becomes `blocked` with a diagnostic until `--unblock`.
- **Crash safety:** every file is one transaction with a checkpoint; a killed pass resumes where it
  stopped.
