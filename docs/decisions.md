# Decisions

These are the maintainers' decisions about the project. They bind until a new decision replaces
them; record each change here with its date.

## 2026-11-10: direction

| # | Decision |
| --- | --- |
| D1 | **Our own mirror of the official sources**, no dependency on a paid API. Commercial register APIs charge per request or per year. The free Statistical Office RPO API that many tools used has been gone since RPO moved to the Ministry of the Interior on 2026-06-01, and many open-source integrations broke with it |
| D2 | **Open source, self-hosted.** We publish code only, never data dumps, and we run no public anonymous instance. Every deployment downloads the data itself, and its operator is the GDPR controller of that copy |
| D3 | Licence **Apache-2.0**. Contributions under DCO, no CLA |
| D4 | **PostgreSQL** is the store, from any provider. Cloudflare D1 and similar platform-only stores were rejected because they cannot be self-hosted |
| D5 | The product is a **search and lookup API**. One smart input box takes a name, an IČO, a DIČ or an IČ DPH. Search ignores diacritics, tolerates typos and matches prefixes |
| D6 | **Name search covers everyone**, sole traders included. The operator is responsible for running it lawfully, and the documentation says so |
| D7 | The **register entry** (court, oddiel, vložka, or the trade-licensing office and number) is returned in a structured form, so that consumers can pre-fill a supplier's legal details. The user confirms them there |
| D8 | **No history.** The register keeps the current state, refreshed daily, with `changed_at` per record and a version hash per section. A consumer detects changes by comparing versions |
| D9 | The Financial Administration API key is **optional**. Each operator requests its own key; the daily ZIP exports need none |
| D10 | Own repository, own migrations (goose) |
| D11 | The name is **Accquix Register**; the repository is `accquix/register` |

## 2026-11-10: stack and design (agreed direction; implementation not started)

| # | Decision |
| --- | --- |
| D12 | **Go** for the whole service, one binary `accquix-register` with the modes `serve`, `sync`, `migrate` and `openapi`. Libraries: Huma v2 on chi, pgx v5, sqlc, goose. Rust would be chosen only if measurements show Go missing its memory or CPU budget |
| D13 | Search uses only `pg_trgm` and B-tree indexes. There is no external search engine. Normalisation runs in Go (refined by D17, D18, D29) |
| D14 | `serve` and `sync` are **independent modes** of one image. They can be deployed and scaled separately |
| D15 | **Cloud-independent.** The reference deployment is `docker compose`. Any container platform with scheduled jobs and any PostgreSQL works |

## 2026-11-10: scope

| # | Decision |
| --- | --- |
| D16 | **RÚZ is not part of v1.** For search and lookup it adds almost nothing: identity, legal form and dates come from RPO, and DIČ from the FS list `ds_dsrdp`. Only 3 261 active RPO legal persons (0.65 %) have no DIČ match in `ds_dsrdp` (measured in the spike). Its first load is about 54 h of API calls and needs its own resumable sync. RÚZ stays a v2 candidate for an optional `accounting` section (financial statements, size, revenue), if a consumer needs it |

## 2026-11-10: design after the measurement spike

Based on [spike-results.md](spike-results.md). These close OQ-1, 5–9, 11, 13–15 and 17–20.

| # | Decision |
| --- | --- |
| D17 | **Default search = adaptive v2**, as measured in the spike's third pass. The name-prefix step always runs. The word-prefix step (every query word is the start of a name word, in any order) runs when the name prefix gives fewer than 10 hits or the rarest query word is rare (threshold configurable, initially 1 000 occurrences). It is driven by its rarest word and capped (initially 1 000 candidates). Ranking: exact name, all query words as whole words, name starts with the query, shorter name. **No invented popularity signal** |
| D18 | **Fuzzy search = word-dictionary correction.** Only unknown query words are corrected against the dictionary of distinct name words (trigram, then edit distance with adjacent transpositions), and then run through the default word search. It is **explicit only** (`mode=fuzzy`), never automatic. It is an option, `REGISTER_SEARCH_FUZZY`, default `true`. When it is off, the API answers `fuzzy_disabled`. It has a per-key rate limit, a limit on concurrent fuzzy queries, `statement_timeout`, and `429` without queueing; all are configurable. **The concurrency limit is per instance**: with several `serve` replicas the operator sets a conservative per-replica value, or adds a shared limit, so that the total load on the database stays bounded. The documentation states: *"Fuzzy search is optional, potentially CPU-intensive, and never substitutes exact identifier matching."* |
| D19 | **`GET /v1/capabilities`** reports what this instance offers: fuzzy on or off, limits, API version. `/v1/sources` stays about data freshness only |
| D20 | **Identifiers are matched exactly, never fuzzily.** The IČO check digit (mod 11) is a **warning only**: 778 real RPO IČOs fail it, 16 of them active. An IČO present in RPO is always returned. There is no DIČ checksum until a reliable source confirms one. **An IČ DPH is not a unique subject key**: VAT groups (§4b) share one, so a lookup by IČ DPH may return several subjects |
| D21 | **Subject model.** One subject per valid IČO, with **`registrations[]`** for all its RPO records. The display name comes from the Commercial or Trade Register record, else from the most recently changed record. **Every distinct official name of the subject's active records is searchable.** A record without a valid IČO keeps its own identity by its RPO `id`. An identifier field that is not 8 digits is treated as "no IČO" |
| D22 | **Publication.** The initial bootstrap blocks the API (`/readyz` not ready, `503 reason=bootstrapping`) until all sources are first published and the indexes are built. **The daily sync never blocks a running API**: each unit commits atomically, and the previous consistent data stays served until a unit commits. A repeated full rebuild either loads into separate tables and switches atomically, or runs in an explicitly announced maintenance mode. It never serves a partial state as complete. A stale source does not stop the API: known records are returned with their as-of date, and an unconfirmed absence is `unknown` |
| D23 | **Validation blocks publication.** When an update breaks a threshold, that update is not published, the previous data stays, and the source is marked `blocked` with a diagnostic in `/v1/sources`, plus an alert. **Nothing is truncated or partly applied.** Initial thresholds, all configurable: a daily RPO file has at most 20 000 records; snapshot parts have 100 000 except the last, and the snapshot is at least 98 % of the previous one; an FS list drops by at most 3 % (`ds_dsrdp`, `ds_dphs`, `ds_iz_ran`) or 20 % (small lists); absence marking is blocked above 0.5 % of rows |
| D24 | **Freshness and certainty.** Thresholds: RPO 3 days, FS 2 days, `ds_iz_ran` 14 days. Each section is `present`, `absent` (the source is fresh and has no row) or `unknown` (never loaded or stale), with `asOf`. Booleans derived from a section, such as `vat.payer`, are `true`/`false` only from a fresh `present`/`absent`; otherwise `null` |
| D25 | **VAT section.** It carries `icDph`, `paragraph`, `payer` (from the paragraph: §4, §4b, §5 are payers; §7 and §7a are registered but are not payers), `registeredOn` (nullable), `payerFrom` (nullable; always null for §7/§7a), `typeChangedOn` and `asOf`. Consumers must not print "VAT payer since" for §7/§7a |
| D26 | **Natural persons without an IČO** (`ds_dsrdp` rows outside RPO) are **never in name search**. The exact DIČ lookup returns only the DIČ, name and municipality, only to authenticated API keys, and is unavailable when anonymous access is enabled |
| D27 | **Change detection by batch polling** of section versions (`lookup:batch`). There is no `changes?since=` feed. **Versions are deterministic content hashes**, so a rebuild from the sources yields the same versions |
| D28 | **No published client SDK in v1.** Consumers generate types from the committed `openapi.yaml` |
| D29 | **Indexes.** B-tree for identifiers, the name prefix (`text_pattern_ops`) and the word tokens. The only trigram GIN index is on the word dictionary. There is **no GiST and no trigram index on full names** |
| D30 | **Implementation is approved** (2026-11-10), following [architecture.md](architecture.md). It is developed and tested in isolation: no production deployments, no changes to any consumer's databases. These must finish before the first production use, in parallel with development: a test on a real small managed tier (import, fixed-rate load, peak disk; OQ-21), relevance regression tests (OQ-16), and the full RPO chain test after the December snapshot (OQ-2) |
