# Contributing

Thank you for helping. Accquix Register is maintained by Accquix and developed in the open.

## Ground rules

- **Code only, never data.** Never commit register records, dumps, downloaded files, or real names
  or identifiers in tests. Test data is synthetic: the `internal/testkit` helpers produce fictional
  names, IČOs in the `9990xxxx` range and DIČs in the `20999xxxxx` range, which do not exist in
  the registers.
- **Read [docs/decisions.md](docs/decisions.md) first.** The decisions bind; propose a change in an
  issue instead of working around one in code. Open questions are in
  [docs/open-questions.md](docs/open-questions.md).
- **Identifiers are exact**, **a stale mirror never says "not found"**, **no personal data beyond
  what identifies a business**, **logs carry no queries or identifiers** (see `CLAUDE.md` and the
  architecture).

## AI-assisted development

Much of this project is written with AI coding agents (Claude Code), reviewed and directed by the
maintainers. We say so openly. Contributions made with AI assistance are welcome under the same
rules: you are responsible for what you submit, it must pass the tests, and you must be able to
explain it.

## Developer Certificate of Origin

We use the [DCO](https://developercertificate.org/), not a CLA. Sign off every commit:

```sh
git commit -s -m "fix: ..."
```

Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/) (`feat:`,
`fix:`, `docs:`, `test:`, `chore:` …).

## Development

Requirements: Go 1.27+ and Docker (for PostgreSQL in tests).

```sh
docker run -d --name register-test-pg -p 5432:5432 -e POSTGRES_PASSWORD=test postgres:18
export REGISTER_TEST_DATABASE_URL="postgres://postgres:test@localhost:5432/postgres?sslmode=disable"
go test ./...
```

Database tests create and drop their own databases; without `REGISTER_TEST_DATABASE_URL` they are
skipped. Before a pull request:

```sh
gofmt -l .                     # must print nothing
go vet ./...
golangci-lint run ./...        # v2, config in .golangci.yml
go run ./cmd/accquix-register openapi > api/openapi.yaml   # when the API changes
```

CI runs the same checks, plus `govulncheck` and a compose smoke test.
