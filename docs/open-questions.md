# Open questions

Record new questions here. Never decide them in code. When a question is answered, move the answer
to [decisions.md](decisions.md) with its date.

OQ-1, 5–9, 11, 13–15 and 17–20 were answered on 2026-11-10 (D17–D30); OQ-30 on 2026-11-11 (D36). OQ-2, 16 and 21 must be done
before the first production use (D30); OQ-3, 4, 10 and 12 are deferred. OQ-37 was answered on 2026-11-11 (D38). OQ-25, 26 and 29 (security
review) were answered on 2026-11-10 (D31–D33).

| ID | Question | Proposal |
| --- | --- | --- |
| OQ-2 | **Measurement spike.** Done 2026-11-10, see [spike-results.md](spike-results.md). Open part: the full chain test (snapshot → daily → reconcile) was impossible because the 2026-10-03 snapshot was already gone | Repeat the chain test after the December snapshot (about 2026-12-05): bootstrap 11-07, apply the dailies, reconcile against the December snapshot |
| OQ-3 | Ask the Ministry of Justice (servicedesk.mssr@justice.sk) about the licence and limits of the ORSR REST API, before using it beyond single on-demand lookups | Yes, before v2 |
| OQ-4 | The new base URL of the RPO REST API under MV SR is undocumented. It is needed only for an on-demand refresh, because the bucket covers the mirror | Ask MV SR, or skip it |
| OQ-10 | A live "check now" refresh through the FS Lists API (optional key) | v2 |
| OQ-12 | Copyright holder in NOTICE: "The Accquix Register Authors" for now. Switch to the company name once the company exists? | Yes |
| OQ-16 | **Search relevance test set** (exact name first, shorter names first, typos, same names of different subjects) | Synthetic fixtures in the repo; real-data checks run locally only |
| OQ-21 | **Capacity on a 1/8–1/4 vCPU database:** about 30 vs 75–140 default searches a second when saturated (Docker limits) | Confirm on the real tier with an open-loop test |
| OQ-22 | **sqlc and gocron (D12) are not used.** The queries are dynamic or bulk (COPY, staged merges, the adaptive word step) and are covered by integration tests against PostgreSQL. Scheduling is `sync --every <interval>` (a loop under the advisory lock) or an external scheduler running `sync` | Drop sqlc and gocron from D12; keep pgx with integration tests |
| OQ-23 | **`active=false` search and keyset `cursor` paging** (architecture v1) are not implemented: the search layer holds active subjects only, and results are capped at 20 | Defer to v1.1, if a consumer needs them |
| OQ-24 | **Publication and contribution security.** On GitHub Free, branch protection, rulesets, fork-PR approval and private vulnerability reporting exist only for public repositories (the API refuses them while the repository is private) | Follow [publication-checklist.md](publication-checklist.md) at publication: private reporting, `main` protection with 0 required reviews, admin-only `v*` tags, CI approval for all external contributors |
| OQ-27 | **Date rules of the sources (D35).** `exportDate` equalled the date in the file name in every RPO file checked on 2026-11-10 (four daily files, two snapshot parts), so the default tolerance is 1 day; FS list dates may be at most one day ahead | Confirm on more files during the OQ-2 chain test; widen `REGISTER_RPO_DATE_SKEW_DAYS` only if it shows a real skew |
| OQ-28 | **Floors of a first publication (D34)** are static numbers from the 2026-11-10 volumes: RPO 2 200 000 records; FS rows 100 000 / 500 000 / 200 000 / 10 000 / 1 000 / 300 | Review yearly, or replace with a ratio to an independent count if the sources ever publish one |
| OQ-31 | **Demo mode with synthetic data.** Trying the API today needs a full sync (about 15 minutes, 2 GB). A command or compose profile that loads synthetic subjects from `internal/testkit` in seconds would serve first-time users and consumers' CI. The data must be unmistakably marked as demo (sources state, attribution) and the mode must never mix with real data in one database | Highest DX value; design the marking first, then implement |
| OQ-32 | **Type generation examples instead of an SDK (D28).** The integration page shows ready commands for TypeScript (`openapi-typescript`), Go (`oapi-codegen`) and Python, about ten lines each | Documentation only; do it with the next site update |
| OQ-33 | **Install from a release.** The image on GHCR, plus `compose.yaml` and `make-secrets.sh` as release assets, so an instance starts without cloning the repository | Part of the first public release (publication-checklist.md) |
| OQ-34 | **Deployment skill for AI agents.** A skill file (served with the documentation site next to `llms.txt`) that walks an agent through installing and checking an instance | After OQ-33, so the skill points at release assets |
| OQ-35 | **API stability policy and CHANGELOG.** What counts as a breaking change of the OpenAPI contract (and of the MCP tools, D36), how deprecation is announced, and a changelog per release | Write before the first public release |
| OQ-36 | **Objections across records that never shared an IČO.** An objection covers the records linked to it by an IČO (D31). Another record of the same person with no IČO, or under a different one, is not covered until the operator names it | Needs a legal view: is a per-record objection enough, or must the operator search for the person's other records? |
