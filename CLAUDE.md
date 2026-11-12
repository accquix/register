# Accquix Register: rules for agents

Accquix Register is an **open-source, self-hosted** service. It mirrors Slovak public business
registers (RPO and the Financial Administration lists) into PostgreSQL and serves a small
read-only HTTP API: one smart search box, and a lookup by IČO, DIČ or IČ DPH. It is a standalone
public project. Licence: Apache-2.0.

**Write everything as if the repository were already public**, because its whole git history will
be. Never commit anything about a particular operator's infrastructure, private repositories,
customers, credentials or internal plans. Keep that in the operator's own private repository.

**Status (2026-11-10):** **implementation approved** (D30), following
[docs/architecture.md](docs/architecture.md). Develop and test in isolation: no production
deployments, and never touch any consumer's databases. Before the first production use, OQ-2,
OQ-16 and OQ-21 must be done.

Read before any work:

1. [docs/decisions.md](docs/decisions.md): the maintainers' decisions. They bind; never re-open
   them silently.
2. [docs/architecture.md](docs/architecture.md): the v1 design (data model, API, sync, deployment).
3. [docs/sources.md](docs/sources.md): sources, URLs, licences, measured volumes, quirks.
4. [docs/open-questions.md](docs/open-questions.md): what is still open. Record new questions
   there, never decide them in code.

## Non-negotiable rules

- **Code only, never data.** The repository never contains real register records: no dumps, no
  real names or IČOs in fixtures. Test fixtures are synthetic, with fictional names and IČOs that
  pass the checksum but are fake, in the real file layout. Every deployment downloads the data from
  the state sources itself.
- **No history.** The register keeps the current state, plus `changed_at` and `absent_since`. A
  record that disappears from a list is an observation, never a legal cancellation date.
- **A stale mirror never says "not found".** `404 not_found` only when every consulted dataset is
  fresh; otherwise `503 unavailable` or a `stale` flag. Never fabricate certainty.
- **Personal data minimisation.** Natural persons, mostly sole traders, are about 80 % of RPO. We
  store and return only what identifies a business:
  - Stored and returned: the business name, the identifiers, the place of business, the register
    entry, the status.
  - Never stored: residence when it differs from the place of business, birth data, statutory
    bodies, stakeholders, representatives.
  - The one exception, by maintainer decision D57: a taxpayer without an IČO (a DIČ-only row of
    the Financial Administration's DIČ list, mostly a private person) has the address that list
    publishes. It is stored with the row and returned for an exact DIČ with an API key (and by the
    optional full-name search, D56), never with anonymous access; the documentation warns the
    operator of the GDPR duties that come with it.
  - Logs never contain the search query or identifier values.
- **Identifiers are exact.** Never match an IČO, DIČ or IČ DPH fuzzily. The IČO check digit is a
  warning only. An IČ DPH can belong to several subjects (VAT groups).
- **VAT status:** §7 and §7a are registered but are not payers; `payerFrom` is null for them. A
  boolean such as `vat.payer` is `true`/`false` only from fresh data, otherwise `null`.
- **Licences.**
  - Attribution is part of every API response: RPO is CC BY 4.0; "Zdroj údajov: Finančná správa
    SR".
  - Never ingest `ds_dph_iban` or `ds_dph_oud` (CC BY-NC-ND).
  - Never mirror VIES: its terms forbid it.
  - ORSR is on-demand only, until its licence is known.
- **Any PostgreSQL.** It must run on plain PostgreSQL 16–18, Neon, PlanetScale Postgres, Supabase
  and RDS.
  - Only `pg_trgm` is required. Normalisation (diacritics, case, legal-form suffix) runs in Go, so
    `unaccent` is not needed.
  - No provider-specific extensions.
  - PlanetScale's `pg_strict` blocks `UPDATE`/`DELETE` without `WHERE`: always write a `WHERE` and
    empty staging with `TRUNCATE`.
- **The sync can die at any moment.** Each unit (one file, one part, one list version) is one
  transaction: TRUNCATE staging, COPY, validate, merge, checkpoint. A rerun is a no-op. Only one
  sync runs at a time, through an advisory lock on a direct (non-PgBouncer) connection.
- **No silent fallback.** A missing optional credential, such as the FS API key, gives an explicit
  `not_configured`, never a fake success.
- **Standalone.** The service has no dependency on any Accquix product. Consumers use only its
  versioned OpenAPI contract.

## Stack (agreed for v1, see decisions)

- **Language and libraries:** Go (1.27+), Huma v2 on chi (OpenAPI 3.1, generated and committed),
  pgx v5 (binary COPY), sqlc, goose (embedded SQL migrations), gocron v2 plus a PostgreSQL advisory
  lock.
- **One binary, `accquix-register`,** with the modes `serve`, `sync`, `migrate` and `openapi`.
- **Self-host:** `compose.yaml` runs `postgres:18` (healthy), then a one-shot `migrate`, then
  `serve` + `sync`. Configuration comes from environment variables only, with `_FILE` variants for
  secrets.
- **No telemetry.**

## Git and attribution

- Commits carry **no** Claude co-author trailer: `.claude/settings.json` sets `attribution` to
  empty. CONTRIBUTING says openly that AI agents assist with development.
- Conventional commit messages (`docs:`, `feat:`, `fix:` …).
- Contributions use DCO sign-off, with no CLA.
- Never push without the maintainers' go-ahead.
- Never commit secrets, real data or downloaded source files.
