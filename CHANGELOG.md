# Changelog

Every release of Accquix Register, newest first. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org). What counts as a breaking change, and how a change is
announced, is in the [stability policy](docs/stability.md).

## [Unreleased] — 1.0.0-rc.1

The first release candidate. The contracts below are frozen for 1.0.0: between this candidate and
the release they change only to fix a defect. Verified on the real data through 2026-11-11; the
first monthly reconcile against a new RPO snapshot (about 2026-12-05) is the last check before
1.0.0.

### Added

- **Sync** of the Register of Legal Entities (RPO): bootstrap from a whole, settled snapshot, the
  daily files after it, and the monthly reconcile against the next snapshot, one transaction per
  file with a checkpoint, so a sync can be killed at any moment and resumes.
- **Sync** of six Financial Administration lists: VAT payers (`ds_dphs`), the DIČ list (`ds_dsrdp`),
  the reliability index (`ds_iz_ran`) and three risk lists (`ds_dphz`, `ds_dphv`, `ds_dpho`).
- **Validation thresholds** on every file and list: a suspicious update blocks its source and keeps
  the previous data, instead of being published (D23, D34, D35). Items without any identifier are
  left out and counted; above a cap that follows each list's size (0.05 % of the DIČ list, at
  most 1000; 0.01 % of the others, at most 100; never less than 10) they block it, and a sudden jump
  within the cap is applied with a warning (D38, D53). Items of the DIČ list with an IČO and no DIČ
  yet (new businesses) are left out and counted apart, with no limit (D52).
- **Search** `GET /v1/search`: one box for a name, IČO, DIČ or IČ DPH; diacritics and word order
  ignored, the start of any word matched; an adaptive default mode and an optional fuzzy mode.
  Every result carries the full address (D37). The exact DIČ of a taxpayer without an IČO gives a
  card of `kind: taxpayer` with the DIČ, name and municipality only, as the lookup does (D54).
- **Taxpayer name search**, off by default (`REGISTER_SEARCH_TAXPAYERS`): taxpayers without an IČO
  by their full name only, in the default mode, with an API key, within a separate per-key limit;
  the response field `taxpayers` says whether they were searched (D56).
- **Lookup** `GET /v1/lookup/{id}` by IČO, DIČ or IČ DPH, with sections `identity`, `tax`, `vat`
  and `risk`, each `present`, `absent` or `unknown`, with its source, as-of date and a content
  version; `POST /v1/lookup:batch` for up to 100 identifiers, with `fields: "versions"` for daily
  change checks.
- **Freshness** `GET /v1/sources` (with `items`, `skippedItems` and `itemsWithoutDic` per list) and **capabilities** `GET /v1/capabilities`; `/livez` and `/readyz`.
- **MCP adapter** for AI agents at `/mcp`, off by default (`REGISTER_MCP`), with three read-only
  tools and the same keys, limits and logging rules as REST; `accquix-register mcp --url` for local
  clients over stdio (D36).
- **Objections**: keep a subject out of name search (`suppress --subject`), with a setup gate so
  that a rebuilt database never serves before its objections are back (D31–D33). An objection is
  linked to the RPO records it covers; each link records why it exists and from which file, and a
  linked record stays hidden when it loses or changes its IČO. `suppress --list --id N` shows the
  links, `suppress --release --id N --record R` excludes one wrongly linked record for good
  (`--unrelease` withdraws it), and `suppress --undo --id N` revokes one objection (D42). The rules
  are fixed in `docs/objection-invariants.md`. A taxpayer without an IČO objects by DIČ
  (`suppress --subject <DIČ>`), which keeps them out of taxpayer name search (D56).
- **Objection backup**: `suppress --export --output FILE` and `suppress --restore FILE`. The file
  (format `objections-v3`) is tab-separated with a count and checksum trailer and carries the
  links, releases and revocations. An export is atomic and replaces a file only when the file is a
  valid backup that the new one contains; a restore only adds and never re-activates a revoked
  objection.
- **Demo mode**: `accquix-register demo` and `deploy/demo/compose.yaml` serve the API over 180
  fictional subjects in seconds, clearly labelled, and the sync refuses a demo database (D41).
- **Overload protection**: at most as many data requests at once as the connection pool has
  connections; the excess gets `503` with `reason: overloaded` and `Retry-After` (D39).
- **Deployment**: one binary with the modes `serve`, `sync`, `migrate`, `suppress`, `demo`, `mcp`
  and `openapi`; a container image; docker compose with separate database roles for migrate,
  sync, the operator and the API; a compose variant for Coolify, Dokploy and similar platforms.
- **Any PostgreSQL 16–18** with only `pg_trgm`: tested on plain PostgreSQL and on PlanetScale
  Postgres with `pg_strict` on.
- **Release assets**: binaries for Linux and macOS (amd64, arm64), the image
  `ghcr.io/accquix/register` for amd64 and arm64 with an SBOM and build provenance, and
  `accquix-register-deploy_<version>.tar.gz` with the compose, PaaS and demo files pinned to the
  released image (D49).

### Changed defaults

- The API's connection pool is 10 connections unless `pool_max_conns` is set in
  `REGISTER_DATABASE_URL` (D40).
- API keys have no rate limit by default; `REGISTER_RATE_PER_SECOND`, `REGISTER_RATE_BURST` and
  `REGISTER_FUZZY_EVERY` set one. Anonymous callers keep theirs per client IP (20/s, burst 40, one
  fuzzy search per 5 s), now under `REGISTER_ANONYMOUS_*` (D55).

### Security

- Personal-data minimisation: no residence, birth data, statutory bodies or representatives are
  stored; logs never carry search queries or identifier values.
- API keys are stored only as SHA-256 hashes and compared in constant time; optional per-key rate
  limits; anonymous access is off by default, limited per client IP, and cannot look up natural
  persons by DIČ (D26).
- The MCP endpoint refuses cross-origin browser requests (DNS rebinding).
- The API's database role can only read; `serve` refuses to start on an outdated schema;
  `migrate` verifies the effective privileges of the roles, and an upgrade that changes what is
  hidden rebuilds the search layer before serving (D45).
- A snapshot is published only from one consistent set of content: a part replaced while it is
  applied restarts the bootstrap or reconcile (D44).
- Source files are parsed within fixed limits on download size, decompressed size, record size and
  counts; the parsers of untrusted input have fuzz tests (D46).
- Supply chain: every GitHub Action is pinned to a commit SHA, Dependabot updates modules, Actions,
  base images and npm packages, and CI scans the image for fixable HIGH and CRITICAL
  vulnerabilities (D47).
- Objections are changed only by a separate operator role; the sync role cannot read their
  reasons, and the API role sees which keys are objected, nothing else (D33).
