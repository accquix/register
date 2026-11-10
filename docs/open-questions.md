# Open questions

Record new questions here. Never decide them in code. When a question is answered, move the answer
to [decisions.md](decisions.md) with its date.

OQ-1, 5–9, 11, 13–15 and 17–20 were answered on 2026-11-10 (D17–D30). OQ-2, 16 and 21 must be done
before the first production use (D30); OQ-3, 4, 10 and 12 are deferred.

| ID | Question | Proposal |
| --- | --- | --- |
| OQ-2 | **Measurement spike.** Done 2026-11-10, see [spike-results.md](spike-results.md). Open part: the full chain test (snapshot → daily → reconcile) was impossible because the 2026-10-03 snapshot was already gone | Repeat the chain test after the December snapshot (about 2026-12-05): bootstrap 11-07, apply the dailies, reconcile against the December snapshot |
| OQ-3 | Ask the Ministry of Justice (servicedesk.mssr@justice.sk) about the licence and limits of the ORSR REST API, before using it beyond single on-demand lookups | Yes, before v2 |
| OQ-4 | The new base URL of the RPO REST API under MV SR is undocumented. It is needed only for an on-demand refresh, because the bucket covers the mirror | Ask MV SR, or skip it |
| OQ-10 | A live "check now" refresh through the FS Lists API (optional key) | v2 |
| OQ-12 | Copyright holder in NOTICE: "The Accquix Register Authors" for now. Switch to the company name once the company exists? | Yes |
| OQ-16 | **Search relevance test set** (exact name first, shorter names first, typos, same names of different subjects) | Synthetic fixtures in the repo; real-data checks run locally only |
| OQ-21 | **Capacity on a 1/8–1/4 vCPU database:** about 30 vs 75–140 default searches a second when saturated (Docker limits) | Confirm on the real tier with an open-loop test |
| OQ-22 | **sqlc (D12) is not used.** The queries are dynamic or bulk (COPY, staged merges, the adaptive word step), and are covered by integration tests against PostgreSQL | Drop sqlc from D12; keep pgx with integration tests |
| OQ-23 | **`active=false` search and keyset `cursor` paging** (architecture v1) are not implemented: the search layer holds active subjects only, and results are capped at 20 | Defer to v1.1, if a consumer needs them |

