# Architecture v1

**Status: approved for implementation (2026-11-10, D30).** The binding decisions are in
[decisions.md](decisions.md); the measurements behind them are in
[spike-results.md](spike-results.md). Where this document and a decision differ, the decision wins.

## Shape

- **One repository, one Go binary (`accquix-register`), one Docker image.**
- **Modes:**
  - `serve`: read-only HTTP API;
  - `sync`: downloads and merges the sources;
  - `migrate`: applies the embedded goose migrations;
  - `openapi`: prints the OpenAPI 3.1 document.
- `serve` and `sync` run independently (D14), so a heavy import never takes CPU from API requests.
- **Not in v1:** UI, multi-tenancy, billing, GraphQL, Kafka, Elasticsearch, Redis, a raw-data
  archive, history, RÚZ (D16).

```text
cmd/accquix-register/        main: serve | sync [--source] | migrate | openapi
internal/config/             env + _FILE secrets
internal/normalize/          name_core, legal-form suffixes, addresses, PSČ, identifier classification (pure)
internal/sources/{rpo,fs}/   fetch (parallel parts, Range resume, ETag, retry) + stream-parse into typed rows; no DB
internal/ingest/             staging COPY, validation, hash merge, absence marking, subject and search refresh, checkpoints
internal/lookup/             subject assembly, sections, freshness and certainty rules
internal/search/             default (adaptive), fuzzy (word dictionary), ranking, limits
internal/api/                Huma handlers, auth, rate limits, RFC 9457 problems
internal/store/sqlc/         generated (committed)
db/migrations/*.sql          goose, embedded
db/queries/*.sql             sqlc input
api/openapi.yaml             committed; CI fails on drift
testdata/{rpo,fs}/           SYNTHETIC fixtures only
deploy/compose/              postgres:18 → migrate → serve + sync --every 1h
```

## Data model

There is one current-state table per source, keyed by the source's own key, and a derived
subject/search layer that `sync` maintains in the same unit transaction.

### Source tables

| Table | Key | Content |
| --- | --- | --- |
| `rpo_record` | RPO `id` | `ico` (valid 8 digits, else null; **not unique**), `kind` (legal_person, sole_trader, public_body, other), `name`, `name_core`, `legal_suffix`, legal form code and text, `established_on`, `terminated_on`, address parts (street, building number, normalised PSČ, municipality, country), the current `registration` (register, office, number, valid from) |
| `fs_tax_subject` | DIČ | `ds_dsrdp`: IČO optional, name, municipality, address |
| `fs_vat_payer` | `IC_DPH` plus IČO or normalised name (VAT groups share an IČ DPH) | `ds_dphs`: paragraph, `registered_on`, `type_changed_on`, `payer_from` (empty for §7/§7a), name, city |
| `fs_list_entry` | (dataset, subject_key) | `ds_dphz`, `ds_dphv`, `ds_dpho`, `ds_iz_ran`; `attrs` typed per dataset in Go; per-list dedup rule |

**Columns on every source table:**
- `source_as_of`: the date the agency stamped on the data.
- `row_hash`: SHA-256 over the **exposed** normalised columns, so `changed_at` moves only on a
  change a client can see (in the spike, only 3–36 % of daily RPO records changed one).
- `first_seen_at`, `changed_at`.
- `absent_since` / `removed_at`: an observation, never a legal date.
- `src_date`, `src_rank` (RPO only): the order guard; a snapshot (rank 1) never overwrites a later
  daily file (rank 0).

### Subject and search layer (D21)

- **Subject key** `skey`: the valid IČO, else `r<RPO id>`.
- `search_name(skey, name, name_core, ico, municipality, kind, primary)`: **one row per distinct
  official name** of the subject's active records; `primary` marks the display name (Commercial or
  Trade Register record first, else the most recently changed record).
- `search_token(token, skey)`: every distinct word of 2+ characters over all of a subject's names.
- `search_word(word, freq)`: the dictionary of distinct words with their subject counts; it drives
  the adaptive word step and the fuzzy correction.
- After each unit, `sync` recomputes these rows for the subjects the unit touched.
- Only active subjects are in the search layer; terminated subjects are reachable by exact lookup.
  A subject listed in `search_suppression` (an objection, keyed by subject key) is left out of the
  search layer only.

### Normalisation rules (pure Go, shared by ingest and search)

- `name_core`: NFD, strip combining marks, lower case, punctuation collapsed, legal-form suffix
  (`s.r.o.`, `spol. s r.o.`, `a.s.`, `k.s.`, `v.o.s.`, `š.p.`…) stripped and kept separately.
- **Current value of a time-bound attribute:** the open entry (no `validTo`); if none is open, the
  entry with the latest `validTo`, then the latest `validFrom`. (A terminated subject has every
  attribute closed.)
- **IČO:** exactly 8 digits, else no IČO (RPO carries a text placeholder in about 6 600 records).
  The mod-11 check digit is computed and returned as a warning only (D20).
- **PSČ:** normalised to `NNN NN` (RPO mixes both spellings).
- **FS lists:** one row per key; duplicates are dropped by a documented rule and counted.

### Indexes (D29)

- B-tree on IČO, DIČ, IČ DPH and the subject keys.
- `search_name (name_core text_pattern_ops)` for the name prefix.
- `search_token (token text_pattern_ops, skey)` and `search_token (skey, token text_pattern_ops)`.
- `search_word (word text_pattern_ops) INCLUDE (freq)` and a **trigram GIN on `search_word.word`**
  (about 10 MB). No GiST, no trigram index on full names.

### Not stored

Statutory bodies, stakeholders, representatives, residence when it differs from the place of
business, birth data, previous names and addresses, NACE history.

## API v1

The OpenAPI 3.1 document is generated by Huma and committed as `api/openapi.yaml`. Errors are RFC
9457 `application/problem+json` with a stable `code`. Every response carries `attribution`.

| Endpoint | Purpose |
| --- | --- |
| `GET /v1/search?q=&mode=default\|fuzzy&limit=` | One box: an identifier gives an exact match, anything else a name search. (`active=false` and keyset `cursor` paging are deferred: OQ-23) |
| `GET /v1/lookup/{id}` | 8 digits = IČO, 10 digits = DIČ, `SK` + 10 = IČ DPH; anything else `400 invalid_identifier` |
| `POST /v1/lookup:batch` | Up to 100 ids; `fields=versions` returns only the section versions (change checks, D27) |
| `GET /v1/sources` | Freshness, state (`ok`, `stale`, `blocked`, `bootstrapping`), diagnostics, licence and attribution per dataset |
| `GET /v1/capabilities` | Fuzzy on or off, limits, API version (D19) |
| `/livez`, `/readyz` | Health; no key |

### Search (D17, D18)

- **Identifier queries** (8 digits, 10 digits, `SK` + 10) are exact lookups, never fuzzy (D20).
- **`mode=default`:**
  1. names starting with the query (`search_name` prefix B-tree);
  2. the word-prefix step: every query word must be the start of a word of the name, in any order.
     It runs when step 1 gave fewer than 10 hits or the rarest query word occurs at most
     `REGISTER_SEARCH_WORD_STEP_MAX` times (1 000). The rarest word drives, the others are checked
     per subject, and the candidates are capped at `REGISTER_SEARCH_WORD_CAP` (1 000);
  3. ranking: exact name → all query words as whole words → name starts with the query → shorter
     name. No popularity signal.
  - Queries of 1–2 characters use step 1 only.
- **`mode=fuzzy`** (when enabled):
  1. query words that are a known word prefix stay as they are;
  2. each unknown word is corrected against `search_word` (trigram, then edit distance with
     adjacent transpositions; at most 1 typo up to 4 letters, 2 above; all alternatives at the
     best distance are kept);
  3. the corrected words run through the default word step.
  - Limits: per-key rate limit, per-instance concurrency limit, `statement_timeout`; excess gets
    `429` with `Retry-After`, never a queue. Disabled → `400 fuzzy_disabled`.
- **Result card:** `ico` (nullable), `name` (display, with diacritics and legal form),
  `municipality`, `kind`, `active`, `terminatedOn`, and the matched name when it is not the
  display name. Default `limit` 10, maximum 20. Name search covers active subjects only; a
  terminated subject is found by an exact identifier and flagged `active: false`.
- **Search proposes, only an exact identifier resolves.** Clients never auto-select the first hit.

### Lookup

- **By IČO:** one subject (D21) with the sections:
  - `identity` (RPO): display name, `registrations[]` (register, office, number, valid from, name
    in that record, active), legal form, established and terminated dates, address parts, active,
    `icoChecksumValid`;
  - `tax` (FS `ds_dsrdp`): DIČ;
  - `vat` (FS `ds_dphs`, D25): `icDph`, `paragraph`, `payer`, `registeredOn`, `payerFrom`,
    `typeChangedOn`;
  - `risk` (FS lists): reliability index, §52 lists, special VAT scheme.
- **By DIČ:** via `ds_dsrdp`, then as by IČO when the DIČ has one. A natural person without an IČO
  returns only DIČ, name and municipality, to authenticated keys only (D26).
- **By IČ DPH:** may match several subjects (VAT groups); the response carries `matches[]`.
- **Each section** carries `state` (`present`, `absent`, `unknown`) and
  `source {dataset, asOf, changedAt, version, stale}` (D24). `version` is a deterministic content
  hash (D27).

**Error rules:**
- `404 not_found` only when every consulted dataset is fresh.
- Otherwise `503 unavailable` with a `reason` (`source_stale`, `bootstrapping`, `database`).
- `stale` is a flag on a `200`, not an error.
- `429` carries `Retry-After`.

**Authentication and logging:**
- `REGISTER_API_KEYS="name:sha256hex,…"` holds hashes only; clients send `Authorization: Bearer`.
- Without keys `serve` refuses to start, unless `REGISTER_ALLOW_ANONYMOUS=true` is set explicitly
  (then D26 lookups are unavailable).
- Rate limiting: an in-process token bucket per key, per instance, with a separate stricter bucket
  for fuzzy.
- Logs record the identifier class and the search mode, never `q` or identifier values.

**Cache (allowed, small):** `serve` may keep an in-process LRU of search and lookup results, keyed
by query and invalidated when the published data version changes.

## Sync pipeline

**Unit of work.** One unit is one RPO daily file, one snapshot part, or one FS list version. Each
unit is **one transaction**: TRUNCATE staging, COPY (`pgx.CopyFromFunc`), validate, merge, refresh
the subject/search rows it touched, update the checkpoint.
- A kill rolls the unit back (verified in the spike with `kill -9`).
- A unit already applied with the same ETag or SHA-256 is skipped. If it is re-applied anyway, the
  hash guard writes 0 rows (verified).

**Downloads.** RPO snapshot parts are fetched in parallel (4–8 streams) with `Range` resume and
retries; a sequential download ran at about 1 MB/s per connection. A snapshot is used only when
its manifest and every listed part are present (the manifest appears before the last parts). FS
lists use a conditional GET with the stored ETag.

**Schedule.** Every 1–2 hours: a check costs a few `304`s. RPO daily files can arrive a day late;
FS lists regenerate around 01:36–01:52 UTC.

**RPO states:** `EMPTY → BOOTSTRAP(part n) → DAILY ⇄ GAP_WAIT → RECONCILE(part n) → DAILY`, plus
`BLOCKED` when validation fails.
- **Bootstrap:** load the newest complete snapshot, then the daily files dated after it, then
  build the indexes and the search layer, then publish (D22).
- **Daily:** the next expected date is `applied_through + 1`. If that file is absent, wait. **Never
  apply a later date.** Alert after 24 h. A gap is healed by the next snapshot dated on or after
  it. The bucket keeps daily files about 45 days and in practice only the latest snapshot.
- **Monthly reconcile:** a hash merge of the new snapshot, writing only differences, under the
  order guard. Rows missing from the snapshot get an absence mark, but only below the threshold.

**FS, per list:** conditional GET → SHA-256 check → stream the XML (`encoding/xml`) into staging →
validate (list date not older, row-count drop under its threshold, 0 parse errors) → merge →
absence marks.

**Publication and validation (D22, D23):**
- Initial bootstrap: the API answers `503 bootstrapping` until every source is first published.
- Daily units commit one by one; the API keeps serving throughout.
- A failed validation rejects that update only, keeps the previous data, marks the source
  `blocked` with a diagnostic, and alerts. Nothing is truncated.
- A full rebuild loads into separate tables and switches atomically, or runs in an announced
  maintenance mode.

**Checkpoint tables:** `sync_source` (state, `base_snapshot`, `applied_through`,
`published_as_of`, cursor, diagnostic), `sync_unit` (one row per unit, with ETag, SHA-256, counts,
status), `sync_run`, `rpo_snapshot_seen`.

**Concurrency:**
- Only one sync runs at a time: `pg_try_advisory_lock` on a **direct** connection (session locks do
  not work through PgBouncer in transaction mode). TCP keepalives so that a killed process releases
  the lock.
- Merges take ROW EXCLUSIVE locks and reads take ACCESS SHARE, so they do not conflict (in the
  spike's load test a full `ds_dsrdp` re-merge ran during load without failures).

**Memory:** RPO is streamed record by record (15–28 MB RSS in the spike); FS ZIPs are saved to a
temporary file first, because a ZIP's index is at the end. Set `GOMEMLIMIT`.

## Personal data

- **Minimise at ingest:** a field that is never returned is never stored.
- **Sole traders — returned:** the business name (it contains the person's name; this cannot be
  avoided), the identifiers, the place of business, the register entry, the status and dates.
- **Terminated subjects:** available by exact lookup, left out of the default search.
- **Natural persons without an IČO:** never in name search; exact DIČ only, minimal fields,
  authenticated keys only (D26).
- **Objections:** `accquix-register suppress --subject <IČO>` adds the subject to `search_suppression`
  (and `--undo` removes it). The person stays reachable by exact
  identifier and disappears from name search.
- **The deployer is the controller.** `docs/self-hosting.md` will say so (to write).

## Deployment

**Reference: `docker compose`.**
- Services: `postgres:18` (healthcheck), a one-shot `migrate` (exits 0), then `serve` + `sync`.
- Configuration is environment variables only, with `_FILE` variants for secrets. There are no
  default credentials.

**Sizing (measured in the spike, 2026-11-10):**

| Resource | Minimum | Comfortable |
| --- | --- | --- |
| PostgreSQL disk | 5 GB (about 2.5 GB data incl. search layer, 0.75 GB WAL, headroom) | 10 GB |
| PostgreSQL RAM | 1–2 GB | 2–4 GB |
| PostgreSQL CPU | 1/8 vCPU (about 30 default searches/s saturated) | 1/4 vCPU or more (75–140/s); fuzzy is the CPU-heavy part |
| Register container | 256 MB RAM | 512 MB |
| First run | about 10–20 min, mostly downloading | — |

- **No backups needed:** the database can always be rebuilt from the official sources, and
  versions are content hashes, so a rebuild yields the same versions.
- **Alerts:** daily RPO not applied for 24 h; last applied daily older than 30 days; an FS list
  older than 2 days (`ds_iz_ran` 14); a source `blocked`. `/v1/sources` and `/readyz` expose all of
  them.

**Any container platform** that runs a long-lived HTTP container plus a scheduled job (256–512 MB,
up to about 30 minutes) can host it, against any PostgreSQL. Deployment guides to write:
- **Fly.io:** an always-on machine; the sync as a scheduled or ad-hoc machine; a static egress IP
  if the database allow-lists by IP.
- **Google Cloud Run:** a service plus a Cloud Run Job with Cloud Scheduler.
- **AWS ECS/Fargate:** a service plus an EventBridge-scheduled task.
- **A VM with Kamal or plain systemd timers.**
- **Cloudflare Containers:** possible, but as of 2026-11 the least convenient (deprecated default
  scheduler, 15-minute cron limit, no static egress IP or city pinning).

**Database roles:** an API role with SELECT only, which can go through a transaction pooler; a sync
role with write rights, on a direct connection (advisory lock, COPY); a migration role for DDL. Use
TLS `verify-full` to managed databases.

**Scaling:** add `serve` replicas behind a connection pooler and size the database CPU by
measured load. The fuzzy concurrency limit is per replica (D18).

## Integrating a consumer

- **Search proposes, only an exact identifier resolves.** The user picks a candidate from
  `/v1/search`, and the application then calls `/v1/lookup/{id}`. Never auto-select.
- **Debounce typing** (about 120–250 ms). Offer fuzzy only as an explicit action, for example
  "Nothing found. Search with typos?", and only when `/v1/capabilities` says it is enabled.
- **Store each section's `version`** with your copy of the counterparty. Check daily through
  `lookup:batch` and show the user what changed.
- **Treat `503 unavailable`, `unknown` and `stale` as "unknown", never as "not found".** An outage
  must not block your workflow; retry safe GETs.
- **VAT wording:** print "platiteľ DPH od …" only when `vat.payer` is `true`; §7/§7a subjects are
  registered but are not payers.
- **The register entry pre-fills** a supplier's legal line, and the user confirms it. Building the
  genitive court name and the exact wording is the consumer's job.
- **Query VIES yourself** for EU VAT validity. The register does not mirror it.
