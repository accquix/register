# Open questions

Record new questions here. Never decide them in code. When a question is answered, move the answer
to [decisions.md](decisions.md) with its date.

| ID | Question | Proposal |
| --- | --- | --- |
| OQ-1 | **Start implementation.** It is held until the maintainers approve the design | — |
| OQ-2 | **Measurement spike** before fixing hardware guidance and validation thresholds (see [measurement-spike.md](measurement-spike.md)) | Run it locally. **Time-sensitive:** the RPO bucket keeps files about 45 days. The 2026-10-03 snapshot will probably disappear around 2026-11-17, and it is the only chance to test snapshot → daily files → reconcile against the 2026-11-07 snapshot |
| OQ-3 | Ask the Ministry of Justice (servicedesk.mssr@justice.sk) about the licence and limits of the ORSR REST API, before using it beyond single on-demand lookups | Yes, before v2 |
| OQ-4 | The new base URL of the RPO REST API under MV SR is undocumented. It is needed only for an on-demand refresh, because the bucket covers the mirror | Ask MV SR, or skip it |
| OQ-5 | Leave `ds_dsrdp` rows without an IČO (private taxpayers who are not in RPO) out of name search, reachable by exact DIČ only? Sole traders stay searchable | Yes |
| OQ-6 | Freshness thresholds that mark a section `stale` | RPO 3 days, FS 2 days, `ds_iz_ran` 14 days |
| OQ-7 | Validation thresholds that block publication (row-count drop, absence ratio) | Set them from the spike's measurements |
| OQ-8 | GIN or GiST for the trigram index, and the word-similarity threshold | Measure on real data |
| OQ-9 | Batch version polling, or a `changes?since=` feed? A feed would let any API key enumerate the whole database | Batch polling in v1 |
| OQ-10 | A live "check now" refresh through the FS Lists API (optional key) | v2 |
| OQ-11 | Publish a TypeScript client package, or let consumers generate types from `openapi.yaml`? | Consumers generate their own (v1) |
| OQ-12 | Copyright holder in NOTICE: "The Accquix Register Authors" for now. Switch to the company name once the company exists? | Yes |
