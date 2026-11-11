# Stability policy

**Status:** in force from the first release, `v1.0.0-rc.1` (D43, answers OQ-35).

This page says what a consumer can rely on across versions, what counts as a breaking change, and
how a change is announced. It covers the API that applications call, and the parts of the program
an operator scripts against.

## Versions

- Releases follow [Semantic Versioning](https://semver.org): `vMAJOR.MINOR.PATCH`. The binary, the
  container image `ghcr.io/accquix/register` and the documentation carry the same version.
- **PATCH:** fixes only, nothing a consumer has to change.
- **MINOR:** new, compatible features (see below). Upgrading needs no change on the consumer side.
- **MAJOR:** a breaking change of a contract listed below.
- **Release candidates** (`1.0.0-rc.N`) freeze the contract of the coming release. Between a
  release candidate and the release, a contract changes only to fix a defect, and the changelog says
  so.
- The HTTP API has its own major version in the path, `/v1`. A new API major version is served
  next to the old one (`/v2` beside `/v1`) for the deprecation period, so a program upgrade never
  forces an API change on its consumers.

## What is a contract

| Contract | Defined by | For |
| --- | --- | --- |
| The HTTP API under `/v1` | [api/openapi.yaml](../api/openapi.yaml) | applications |
| The MCP tools | [api/mcp-tools.json](../api/mcp-tools.json) | AI agents |
| The command line: modes, flags, exit codes | `accquix-register help` and [configuration.md](configuration.md) | operators |
| The configuration: `REGISTER_*` variables and what they mean | [configuration.md](configuration.md) | operators |
| The objection backup file | [configuration.md](configuration.md) (`suppress --export`) | operators: a backup restores into every later version |
| The meaning of the answers | [architecture.md](architecture.md) and the API documentation | everyone |

The meaning of the answers is part of the contract, not only their shape: `404` only from fresh data,
`unknown` never as "not found", `vat.payer` `true` or `false` only from fresh data, §7 and §7a not
payers, section versions as content hashes that do not change when the data does not.

**Not a contract:**

- the database schema: only `migrate` changes it, and consumers read the data through the API;
- the Go packages under `internal/`;
- the log format: fields can change, but logs never carry queries or identifiers;
- the order and the exact set of search results: relevance improves between versions. Search
  proposes; only an exact identifier resolves;
- the text of `title` and `detail` in errors, and of `instructions` and tool descriptions in MCP;
- default values of limits and timeouts. A changed default is listed in the changelog.

## Compatible changes (MINOR or PATCH)

- a new endpoint, MCP tool, mode, flag or variable;
- a new optional request parameter;
- a new field in a response. **Consumers must ignore fields they do not know**;
- a new value of an **open** enumeration: the problem `code` and `reason`, the `attribution` texts,
  the list of sources in `/v1/sources`. Consumers must handle an unknown `code` by its HTTP status,
  and an unknown `reason` as "unavailable for now";
- a new optional field of the objection backup file, which older restores refuse with a clear error
  rather than misread;
- a stricter check that rejects input the documentation already called invalid.

## Breaking changes (MAJOR, and `/v2` for the HTTP API)

- removing or renaming an endpoint, a field, a parameter, an MCP tool, a flag or a variable;
- changing the type or the meaning of a field, or making an optional parameter required;
- a new value of a **closed** enumeration: a section `state` (`present`, `absent`, `unknown`), a
  lookup `type`, a subject `kind`, the search `mode`;
- changing when `404` is returned, or any rule of the meaning of the answers above;
- changing how section versions are computed, which would report every subject as changed;
- requiring a credential, a role or a database extension that was not required before.

A security fix may need a breaking change sooner than the deprecation period allows. It is then
made in a PATCH or MINOR release, and the changelog and the security advisory say so.

## Deprecation

1. A contract to be removed is marked first: `deprecated: true` in the OpenAPI document or the
   tool description, the `Deprecation` and `Sunset` response headers on the HTTP API, and an entry
   under **Deprecated** in the changelog with the replacement.
2. It keeps working for at least **6 months** after the release that announced it, and until the
   next MAJOR release.
3. It is removed only in a MAJOR release.

## Upgrades

- Run `migrate` before the new `serve` and `sync`. `serve` refuses to start on a schema older than
  itself and says so.
- A MINOR or PATCH upgrade keeps the database: no new first run, and section versions stay the same
  for the same data.
- A downgrade is not supported across a migration; `migrate down-to N` exists for development.

## Changelog

Every release has an entry in [CHANGELOG.md](../CHANGELOG.md), grouped as **Added**, **Changed**,
**Deprecated**, **Removed**, **Fixed** and **Security**, with a line for every change of a default
value. The documentation site shows the same entries.

## Supported versions

Fixes go to the latest MINOR release of the current MAJOR version. Security fixes also go to the
latest MINOR release of the previous MAJOR version for 6 months after a new MAJOR release.
