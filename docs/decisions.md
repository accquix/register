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
| D13 | Search uses only `pg_trgm` and B-tree indexes. There is no external search engine. Normalisation runs in Go |
| D14 | `serve` and `sync` are **independent modes** of one image. They can be deployed and scaled separately |
| D15 | **Cloud-independent.** The reference deployment is `docker compose`. Any container platform with scheduled jobs and any PostgreSQL works |
