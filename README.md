# Accquix Register

**Slovak business data. Your infrastructure.**

Accquix Register is a free, open-source service that builds and maintains your own searchable
database of Slovak companies and sole traders from the official public registers, with no
commercial API subscription and no vendor lock-in.

> **Status: design phase.** No release yet. The design is in [docs/](docs/).

## What it does

- **Mirrors the official sources** into your PostgreSQL:
  - the Register of Legal Entities (RPO);
  - the Financial Administration lists: DIČ, IČ DPH and VAT paragraph, risk lists, reliability
    index;
  - the Register of Financial Statements (RÚZ).
- **Keeps the mirror current** with daily incremental updates, resumable after any crash.
- **Serves a small HTTP API** (OpenAPI 3.1):
  - one smart search box: company or trader name, IČO, DIČ or IČ DPH. It ignores diacritics and
    tolerates typos.
  - a lookup that returns the identity, the register entry, the tax and VAT status and the risk
    lists, each with its source and as-of date.
- **Runs anywhere:** one Go binary and plain PostgreSQL (16–18, Neon, PlanetScale, Supabase, RDS…).
  `docker compose up` brings up everything.

## What it is not

- **No data is published here.** The repository contains code only. Your instance downloads the
  data from the official sources, and you are the controller of your copy under GDPR. See
  [DATA_SOURCES.md](DATA_SOURCES.md).
- **No public hosted API.** Run your own.
- **No history.** The register keeps the current state of each record, with the date it last
  changed.
- **Not a VAT validity oracle.** The register reports what the Financial Administration lists say,
  and as of when. For EU VAT numbers, query VIES yourself.

## Who builds it

Accquix Register is developed and maintained by [Accquix](https://accquix.com) and published so
that anyone can run it.

## License

- **Code:** [Apache-2.0](LICENSE).
- **Data:** the data your instance downloads stays under its sources' licences. See
  [DATA_SOURCES.md](DATA_SOURCES.md).
