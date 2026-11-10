<!--
Thank you for the pull request. Before you start, please read CONTRIBUTING.md:
Accquix keeps full discretion over the roadmap and over which contributions are merged, and we
cannot promise an answer to every pull request. For a substantial change, open an issue first.
Security vulnerabilities go through SECURITY.md, never through a pull request or issue.
-->

## What and why

<!-- What does this change, and why? Link the issue if there is one. -->

## Checklist

- [ ] Every commit is signed off (`git commit -s`, DCO).
- [ ] No real register data: tests use the synthetic `internal/testkit` data only.
- [ ] `gofmt -l .` prints nothing, `go vet ./...` and `go test ./...` pass.
- [ ] `api/openapi.yaml` is regenerated if the API changed.
- [ ] Docs updated (`docs/`, `site/docs/` in English and Slovak) if behaviour changed.
