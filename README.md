# Accquix Register

**Slovak business data. Your infrastructure.**

Accquix Register is a free, open-source service that builds and maintains your own searchable
database of Slovak companies and sole traders from the official public registers, with no
commercial API subscription and no vendor lock-in.

> **Status: release candidate, `1.0.0-rc.1`.** The API and the other contracts are frozen for
> 1.0.0 ([stability policy](docs/stability.md)). Verified on the real data, including load tests on
> managed PostgreSQL; the first monthly reconcile against a new RPO snapshot (about 5 December
> 2026) is the last check before 1.0.0. See the [changelog](CHANGELOG.md).

## What it does

- **Mirrors the official sources** into your PostgreSQL:
  - the Register of Legal Entities (RPO);
  - the Financial Administration lists: DIČ, IČ DPH and VAT paragraph, risk lists, reliability
    index.
- **Keeps the mirror current** with the daily change files of the Register of Legal Entities,
  applied within the hour of their publication rather than with the next full snapshot, so a new
  sole trader is found as soon as the ministry publishes the change. Resumable after any crash.
- **Serves a small HTTP API** (OpenAPI 3.1):
  - one smart search box: company or trader name, IČO, DIČ or IČ DPH. It ignores diacritics and
    word order, matches the start of any word, and can optionally correct typos.
  - a lookup that returns the identity, the register entry, the tax and VAT status and the risk
    lists, each with its source and as-of date.
- **Runs anywhere:** one Go binary and plain PostgreSQL (16–18, Neon, PlanetScale, Supabase, RDS…).
  `docker compose up` brings up everything. About 1 GB of RAM and 10 GB of disk for the database;
  a 1/8 vCPU managed database serves about 30 searches a second with headroom
  ([measurements](docs/managed-postgres-results.md)).
- **For AI agents too:** a read-only MCP endpoint with the same keys and limits as the API.

## Try it in seconds

The demo serves the real API over fictional companies, with nothing to download from the
registers:

```sh
git clone https://github.com/accquix/register && cd register
docker compose -f deploy/demo/compose.yaml up -d --wait serve
curl -s 'localhost:8080/v1/search?q=zlata'
```

Every answer says `DEMO DATA` in its attribution. Stop it with
`docker compose -f deploy/demo/compose.yaml down`.

## Run it with the real data

```sh
git clone https://github.com/accquix/register && cd register/deploy/compose
./make-secrets.sh && docker compose up -d
docker compose run --rm operator suppress --init-empty   # a new installation: no objections to restore
```

The first sync downloads about 1 GB and takes 7–20 minutes. Then:

```sh
curl -s -H "Authorization: Bearer <key>" "localhost:8080/v1/search?q=<a company name>"
curl -s -H "Authorization: Bearer <key>" "localhost:8080/v1/lookup/<IČO>"
```

- Requirements, database roles, monitoring and GDPR duties: [docs/self-hosting.md](docs/self-hosting.md).
- API contract: [api/openapi.yaml](api/openapi.yaml); MCP tools: [api/mcp-tools.json](api/mcp-tools.json).
- What stays stable between versions: [docs/stability.md](docs/stability.md).
- Design and decisions: [docs/](docs/README.md).

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
