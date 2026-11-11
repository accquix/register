# Measurement spike results (OQ-2)

> **Outcome:** decisions D17–D30 in [decisions.md](decisions.md) and the approved
> [architecture.md](architecture.md). This file records how we got there, in order. Proposals in the
> earlier sections that the later passes replaced are **superseded**: the narrow full-name
> `rpo_search` table with a trigram GIN, GiST, and an automatic typo pass. The final search is the
> third pass: the adaptive default and dictionary fuzzy.

Run on 2026-11-10 with throw-away code, following [measurement-spike.md](measurement-spike.md).
The code is not part of the repository, and no downloaded data is either. Every number below was
measured unless it is marked as an estimate.

## Setup

- **Data:**
  - RPO snapshot `init_2026-11-07`: 23 parts, 936 MB gzipped, 2 294 939 records.
  - RPO daily files `actual_2026-09-26` … `actual_2026-11-10`.
  - The six FS lists of 2026-11-10: `ds_dphs`, `ds_dsrdp`, `ds_dphz`, `ds_dphv`, `ds_iz_ran`,
    `ds_dpho`.
- **Loader:** Go 1.27.3, pgx v5.12 binary COPY, `encoding/json` token streaming for RPO,
  `encoding/xml` for FS. It ran in a container limited to 1 GiB with `GOMEMLIMIT=600MiB`.
- **Database:** PostgreSQL 18.6 in Docker on an arm64 laptop (Docker VM with 4 vCPU and 7 GB), with
  only `pg_trgm`. It was run twice:
  - **unconstrained:** `shared_buffers=1GB`, `maintenance_work_mem=1GB`;
  - **constrained:** `--cpus=0.25 --memory=2g`, `shared_buffers=512MB`,
    `maintenance_work_mem=256MB`, as a stand-in for a small managed tier.
- **Schema:** close to [architecture.md](architecture.md). There is one table per source, with
  `row_hash` over the exposed columns and the order guard `(src_date, src_rank)`. Each unit is one
  transaction: TRUNCATE staging, COPY, merge, checkpoint. Normalisation (NFD, strip marks, lower
  case, legal-form suffix) runs in Go.
- **Not covered:** RÚZ (out of v1, D16), the HTTP API, a paid managed tier (step 10).

## Deviation: the full daily chain could not be tested

The plan was: bootstrap from `init_2026-10-03`, apply the daily files up to 11-07, then reconcile
against `init_2026-11-07`. **The 2026-10-03 snapshot had already left the bucket** on 2026-11-10,
about a week earlier than expected. The bucket held only `init_2026-11-07` and the daily files from
2026-09-26.

**The substitute test.** The daily files 09-26…11-07 were applied in order into an empty schema.
For every record present in both, the hash of its last daily version was compared with its
snapshot version. The full test can be repeated after the December snapshot (expected around
2026-12-05).

## Results against the pass thresholds

| Check | Pass | Unconstrained | Constrained (0.25 vCPU, 2 GB) |
| --- | --- | --- | --- |
| Loader peak RSS | ≤ 600 MB | **25 MB** | **28 MB** |
| RPO bootstrap, 2.29 M records, no indexes | ≤ 30 min / ≤ 4 h | **77 s** (14–48 k rows/s per part) | **78 s** |
| Index build (all tables) | — | 19 s (trigram GIN 4.4 s) | 4 min 38 s (trigram GIN 2 min) |
| Narrow search table + GIN | — | 3.3 s | 71 s |
| One RPO daily file | ≤ 2 min | 0.06–0.9 s | 0.06–0.85 s |
| Full FS daily set (6 lists, first load) | ≤ 15 min | 53 s | 3 min 2 s |
| Substitute chain check: residual difference | ≤ 0.1 % | **0.003 %** (2 of 58 999) | — |
| Kill and rerun checksum (RPO part, `ds_dsrdp`) | identical | **identical** | — |
| Re-apply a daily file / a snapshot part | 0 written | **0 / 0** | **0 / 0** |
| Core database size | ≤ 5 GB | **2.1 GB** | 2.1 GB |
| Search p95 warm, narrow table | ≤ 50 ms | **33 ms** | **296 ms** ✗ |
| Search p95 cold, narrow table | ≤ 150 ms | **45 ms** | **497 ms** ✗ |
| Prefix (1–2 characters) and exact IČO | — | < 1 ms warm, < 7 ms cold | < 1 ms warm, < 5 ms cold |

**Summary.** The sync side passes everywhere, with a large margin. Name search passes on a full
core, but **fails on 0.25 vCPU**: trigram search is CPU-bound. See "Hosting" below.

## Sync

- **Memory.** The loader stays at 15–28 MB RSS whatever the file size. The 50–150 MB estimate was
  pessimistic. A 256 MB container is enough for `sync`.
- **The bootstrap is loader-bound, not database-bound.** It takes 77 s on both machines, because
  COPY into a table without indexes costs the database little. On the small tier the index build
  dominates (about 6 min in total).
- **The hash guard works.** Of the records in a steady-state daily file, only 3–36 % change a column
  we store: 15 of 536, 43 of 167 and 833 of 2 311 on 11-08…11-10. The rest only change data we do
  not keep (stakeholders, activities). So `changed_at` moves only on a visible change, and WAL
  stays small.
- **The order guard works.** Re-applying snapshot part 005 after the daily files 11-08…11-10 wrote
  0 rows.
- **Units are atomic.**
  - The loader was killed with `kill -9` in the middle of snapshot part 2: part 1 stayed committed,
    part 2 rolled back, the rerun skipped part 1, and the checksum equalled a clean run's.
  - The same happened with `ds_dsrdp` (1.42 M rows) killed at 9 s: 0 rows left, and the rerun's
    checksum was identical.
- **The FS conditional GET works.** It answers `304` in 40–180 ms with the stored ETag.
- **Daily volume:** 131–4 270 records per file, about 2 000 on average (43 files).
- **Disk (constrained run, after it finished):** the volume held 2.9 GB, of which 2.1 GB was the
  database and 0.75 GB WAL. Peak usage during the run was not sampled.

### The two chain differences

Neither is an importer error:

1. **The daily file lags the snapshot by a day.** A change of registration office is already in
   the 11-07 snapshot, but it reaches the daily feed only in `actual_2026-11-08`. The chain then
   converges by itself, and the order guard lets the later daily file win.
2. **A bug in the spike's "current value" rule, which matters for the build.** For a terminated
   subject the snapshot closes *every* attribute (`validTo` set), whereas its last daily version
   still had them open. The spike's rule ("the open entry, else the latest `validFrom`") then picked
   the other of two names. **Rule for the build:** the open entry; if none is open, the entry with
   the latest `validTo`, then the latest `validFrom`. Add a synthetic fixture for it.

## Data shape findings (they change the model)

- **One IČO can have several RPO records.** 22 597 IČOs have more than one *active* record. This is
  almost always a sole trader with one record per source register: the Trade Register plus, for
  example, financial agents, athletes, taxi services, road transport or lawyers. 39 % of those
  records carry different names.
  - Lookup by IČO must merge the records: one subject, a `registrations[]` array, and a name rule.
  - The `rpo_subject` key stays the RPO `id`, with IČO not unique, as planned.
- **A text placeholder instead of an IČO.** 6 603 records carry a 9-letter word in the identifier
  field instead of 8 digits. One such value is shared by 430 active records (lawyers and foreign
  entities). The normaliser must accept only 8 digits and treat anything else as "no IČO".
- **Kinds (current snapshot):**
  - sole traders: 1 617 986 records, 534 118 of them active;
  - active legal persons: 502 818;
  - active in total: 1 041 324.
- **DIČ coverage without RÚZ.** Only 3 261 active RPO legal persons (0.65 %) have no IČO match in
  `ds_dsrdp`. This supports D16.
- **`ds_dsrdp`:** 158 312 rows have no IČO. About 2 500 of them look like legal entities by name;
  the rest are natural persons outside RPO (OQ-5).
- **`ds_dpho`** contains 2 duplicate keys out of 1 004. Each FS list needs a dedup rule.
- **`PLAT_DPH_OD` is empty for every §7 and §7a subject** (81 340 rows). These subjects are
  registered for VAT but are not VAT payers. `DATUM_REG` is always filled. So the `vat` section
  needs both `registeredOn` and a nullable `payerFrom`, plus `payer: true|false` derived from the
  paragraph. A consumer must not print "VAT payer since" for §7/§7a.
- **Every RPO record carried a `sourceRegister`.** So the register entry is available for all
  subjects, sole traders included.

## Search

### Speed

All 30 queries of the test set, by class: 1–2-character prefixes, common words, company names, no
diacritics, typos, personal names, long multi-word, exact IČO. The p50/p95 figures cover the 24
trigram queries.

| Variant (unconstrained) | Warm p50 / p95 | Cold p50 / p95 |
| --- | --- | --- |
| Trigram GIN on the full `rpo_subject` (1.33 GB heap), `terminated_on IS NULL` filter | 20 / 88 ms | 38 / 125 ms |
| **Narrow `rpo_search` (id, name_core; active only; 99 MB with index) + GIN** | **12 / 33 ms** | **17 / 45 ms** |
| GiST KNN `ORDER BY q <<-> name_core LIMIT 10`, full table | 24 / 621 ms | 42 / 770 ms |
| GiST KNN, narrow table | 9 / 270 ms | 33 / 291 ms |

- **GIN wins for this workload.** GiST KNN is fast for short single words, but slow on multi-word
  queries.
- **Never keep a GiST and a GIN trigram index side by side.** With both present, the planner chose
  GiST for `<%`, and the queries became 5–10× slower.
- **The slowest queries** are common words and common personal names: thousands of candidates are
  re-checked and ranked.
- **The query** that was measured:

  ```sql
  SELECT id FROM rpo_search
  WHERE $1 <% name_core                          -- pg_trgm.word_similarity_threshold = 0.6
  ORDER BY (name_core LIKE $2) DESC, word_similarity($1, name_core) DESC, id
  LIMIT 10;
  -- plan: Bitmap Heap Scan on rpo_search ← Bitmap Index Scan on rpo_search_trgm_gin
  ```

  Prefixes of 1–2 characters use a `text_pattern_ops` B-tree with
  `ORDER BY name_core USING ~<~ LIMIT 10`.

### Quality (first look, not a full relevance test)

- **Good:**
  - exact company names come first;
  - queries without diacritics match names with them;
  - the legal-form suffix in the query is ignored.
- **Weak:**
  - **Exact name not always first.** When several names start with the query, the tie is broken by
    `id`, so "X trans" can rank above "X". Rank exact `name_core = q` first, then shorter names.
  - **Typos in short single words fail.** A transposition such as "slovanft" or "volksvagen" scores
    0.57 against the right name, below the 0.6 threshold, so the right subject is missing. A
    multi-word query with the same typo works (0.74).
  - **Possible fix, to measure:** a second pass with a lower threshold (about 0.4) only when the
    first pass returns fewer than N hits.

A relevance test set is needed before v1: expected subject → query → top-k, synthetic names in the
repository, and real-data checks run locally only.

## Hosting implications

- **The sync fits anywhere:** 28 MB RSS, and the bootstrap needs no CPU worth mentioning.
- **The database:** 2.1 GB of core, and 2.9 GB on disk after the constrained run including WAL. A
  10 GB tier has room for a shadow rebuild.
- **Name search is CPU-bound.** On 0.25 vCPU the narrow-table p95 is about 300 ms warm and 500 ms
  cold, about 9× slower than unconstrained.
  - Caveat: a Docker CPU quota is a hard throttle, and a managed "1/4 vCPU" may burst differently.
    It must be measured on the real tier (step 10) before a size is chosen.
  - **Expectation:** the smallest tiers are fine for lookup and sync, but name search needs about
    1 vCPU, or a result cache, to meet 50 ms p95.
- **No external search engine is needed.** PostgreSQL meets the target when it has CPU.

## Proposed changes to the architecture (for the maintainers' approval)

1. **A narrow search table** `rpo_search(id, name_core, …)` with active subjects only and a trigram
   GIN index. The sync maintains it in the same unit transaction. No GiST trigram index.
2. **Subject = IČO over several RPO records.** Lookup merges them into one `identity` with a
   `registrations[]` array. Define a name rule, for example the Trade Register record first, then
   the latest change.
3. **The normaliser:**
   - accepts only 8-digit IČOs;
   - picks the "current value" as: open entry → latest `validTo` → latest `validFrom`;
   - dedups each FS list.
4. **Search ranking:**
   - exact match, then prefix, then shorter name, then `word_similarity`;
   - a low-threshold second pass for short typo queries.

   Both are to be measured.
5. **Bootstrap publication.** The spike wrote the bootstrap into the live table. The API is
   untested here, so it is a design requirement, not a measured fact: while a bootstrap is not
   complete, `/readyz` is not ready and lookup answers `503`. Alternatively, build into shadow
   tables and swap. Never serve 17 of 23 parts as if complete.
6. **Validation thresholds (OQ-7), proposed from these numbers:**
   - snapshot parts have 100 000 records except the last;
   - a daily file has at most 20 000 records (the maximum seen was 4 270);
   - FS row-count drop: at most 3 % for `ds_dsrdp`, `ds_dphs` and `ds_iz_ran`, at most 20 % for
     the small lists;
   - absence marking is blocked above 0.5 %.

## Search mini-spike (2026-11-10, second pass)

The first pass showed that trigram name search is CPU-bound and fails on 0.25 vCPU. This pass
tested cheaper query shapes on the same data, on smaller tiers.

### Setup

- **Subject-level search tables**, built from active RPO records and keyed by IČO (else by RPO
  `id`). One subject per IČO; the name comes from the Commercial or Trade Register record first,
  else the latest record.
  - `search_name(skey, name_core)`: 1 017 893 subjects from 1 041 324 active records. With its
    indexes (primary key, `text_pattern_ops`, trigram GIN of 43 MB) it takes 176 MB.
  - `search_token(token, skey)`: every word of 2 or more characters, 2 374 824 rows (359 749
    distinct), 187 MB with its B-tree `(token text_pattern_ops, skey)`.
  - Build time on the full machine: 13 s for both tables.
- **Tiers**, as Docker limits on the same volume:
  - "PS-20-like": 0.25 vCPU, 2 GB, `shared_buffers=512MB`;
  - "PS-10-like": 0.125 vCPU, 1 GB, `shared_buffers=256MB`.
- **Queries:** the same 30 queries, plus a 27-case relevance set. Each case is a query with the
  expected subject: exact name, partial typing, word order, diacritics, legal-form suffix, typos.
  Only legal-entity names are used.
- **Latency** is the client-side wall time per search, including every tier that ran, over the
  text queries (1–2-character prefixes and identifiers excluded).
- **Search modes:**
  - `base`: trigram only, as in the first pass;
  - `tiered`: token prefix (every query word is the start of some name word, in any order), then
    trigram 0.6 to fill up to 10 hits; if the token tier finds nothing, a typo pass (trigram 0.5,
    100 candidates, re-ranked in Go by edit distance with adjacent transpositions);
  - `prefix`: the name starts with the query (a B-tree range scan);
  - `ptok`: `prefix`, then the token tier if fewer than 10 hits, then the typo pass if still 0.

### Results

p50 / p95 in ms; relevance = the expected subject in the top 3, out of 27.

| Mode | Full machine, warm | PS-20-like, warm / cold | PS-10-like, warm / cold | Relevance top 3 |
| --- | --- | --- | --- | --- |
| `base` (trigram only) | 10 / 33 | 81 / 185 · 98 / 307 | 196 / 691 · 198 / 897 | 23 |
| `tiered` | 8 / 58 | 18 / 308 · 32 / 306 | 106 / 1 297 · 133 / 1 509 | 26 |
| `prefix` (name starts with) | — | 0.1 / 0.1 · 0.5 / 4 | 0.1 / 0.1 · 0.2 / 0.8 | 17 |
| **`ptok`, all queries** | — | 0.6 / 192 · 1.5 / 202 | 1.1 / 591 · 1.5 / 395 | **26** |
| **`ptok`, without the 10 typo-path queries** | — | **0.5 / 1.5 · 1.3 / 12** | **1.0 / 3 · 1.2 / 53** | — |
| `ptok`, typo path only (10 queries) | — | 94 / 485 · 93 / 963 | 104 / 1 698 · 199 / 3 393 | — |

### Findings

- **The fast path costs almost nothing, even on 1/8 vCPU.** Three steps cover 41 of 51 text
  queries, including partial typing, any word order, diacritics and suffixes: exact identifier,
  then name prefix, then word prefixes. They have a p95 of 12 ms cold on the PS-20-like tier and
  53 ms cold on the PS-10-like tier.
- **Fuzzy typo matching is the whole cost.** It needs the trigram index over many candidates: up
  to about 0.5–1 s on 1/4 vCPU and 1.7–3.4 s on 1/8 vCPU.
- **Relevance:**
  - `ptok` and `tiered` both find the expected subject in the top 3 in 26 of 27 cases;
  - name prefix alone in 17 (no word order, no inner words, no typos);
  - trigram alone in 23.
  - The remaining miss is a transposition typo whose right answer is not among the first 100
    trigram candidates.
- **Not measured, a risk for the token tier:** a query of only very common words, not at the start
  of a name, with fewer than 10 name-prefix hits. The candidate sets intersect in full. Before the
  build, add such queries to the set. Mitigation: cap the candidate set, or an edge-n-gram table
  `(prefix, name_len, skey)` that an index can order and stop after 10.

### In-memory prototype (Go, inside the process)

- **Index:** a sorted token list with posting lists, plus a trigram posting index, over the same
  1 017 893 names. It loads from `search_name`.
- **Cost:** 188 MB heap, 390–450 MB RSS. The build takes 5 s at 1 vCPU and 33 s at 0.25 vCPU.

| CPU of the Go process | Fast path p50 / p95 | Typo path p50 / p95 | Relevance top 3 |
| --- | --- | --- | --- |
| 1 vCPU | 0.5 / 6 | 2.7 / 13 | 26 |
| 0.5 vCPU | 0.6 / 12 | 2.7 / 64 | 26 |
| 0.25 vCPU | 1.3 / 94 | 3.1 / 186 | 26 |

- **Typo matching in memory is 30–100× cheaper than in PostgreSQL.**
- **It costs about 0.5 GB RAM per `serve` replica, a second index to keep in sync, and code to
  maintain.**

### Reference: Slovensko.Digital Autoform (documentation read 2026-11-10)

- **It searches "podľa začiatku názvu"**, i.e. by the start of the name, and by full IČO
  ([integration manual](https://ekosystem.slovensko.digital/sluzby/autoform/integracny-manual)).
  Typo handling is not documented.
- **Results:** 5 by default, at most 20.
- **Terminated subjects are included by default**, with `terminated_on`. `filter=active` removes
  them, and the widget shows them struck through.
- **One response carries the whole card:** name, IČO, DIČ, IČ DPH, VAT paragraph, the address in
  parts (street, súpisné and orientačné číslo, PSČ, obec, country), registration office and
  number, legal form, dates.
- **Price:** 60 requests per minute per IP. 9–72 € a month up to 2 000 requests
  ([cenník](https://ekosystem.slovensko.digital/cennik)).
- Its source code was not found in public.
- **KROS** (through FinStat) behaves the same way in the UI: names that start with the query,
  diacritics ignored, a card with name, IČO and municipality.

So the commercial reference is the **name-prefix path**, which costs < 5 ms on any tier. Our word
order and inner-word matching go beyond it at a similar cost. Typo tolerance is an extra that
neither documents.

### Proposed decision for search v1 (for the maintainers)

1. **Default search = `ptok` without the typo pass:** exact identifier → name prefix → word
   prefixes. It runs on the smallest tiers (PS-10-like p95 ≤ 53 ms cold).
2. **Typo matching is separate**, as `fuzzy=true` or only when the default returns 0 hits, for a
   client to call when the user stops typing, not on every keystroke. On a small database it may
   take hundreds of ms; the documentation says so. Moving it into memory is an option for v1.1,
   only if needed (about 0.5 GB RAM, < 15 ms at 1 vCPU).
3. **The search result card** comes from the narrow table without a join: display name with
   diacritics and legal form, IČO, municipality, kind, and a terminated flag. Proposed
   `search_name` columns: `skey, ico, name, name_core, municipality, kind, terminated_on`.
   Terminated subjects stay out of the default (`active=true`); `active=false` includes them,
   flagged.
4. **Database sizing:** the default search no longer drives CPU. A 1/8–1/4 vCPU tier is enough for
   lookup, search and sync. The PS-5-like tier (1/16 vCPU, 512 MB) was not tested; with about 2.5
   GB of data and 512 MB RAM most reads would be cold.

## Search, third pass (2026-11-10): adaptive default, dictionary fuzzy, concurrent load

### Adaptive default search ("v2")

- **The steps:**
  - The name-prefix step always runs.
  - The word-prefix step runs when the name prefix gave fewer than 10 hits, or when the query's
    rarest word is rare enough (at most 1 000 occurrences).
  - The word step is driven by its rarest word and capped at 1 000 candidates. The other words are
    checked per subject through an index on `search_token(skey, token)`.
- **Word frequencies** come from a dictionary `search_word(word, freq)`: 359 749 distinct words,
  46 MB with its indexes.
- **Ranking:** exact name, then all query words as whole words, then "name starts with the query",
  then shorter name.

| Default variant (full machine, 70 queries incl. 19 common-word queries) | p50 / p95 / max | Relevance top 10 (30 cases) |
| --- | --- | --- |
| v1: word step only when the name prefix gives < 10 | 0.3 / 1.4 / 38 ms | 23 |
| Both steps always, cap 3 000 | 0.8 / 18 / 23 ms | 24 |
| **v2 adaptive, cap 1 000** | **0.7 / 5 / 8 ms** | **24** |

- "telekom" now finds "Slovak Telekom" (rank 7; v1 never reached it).
- Very common words ("spol", "jan", "obec") skip the word step when the name prefix already
  fills the page. There is no popularity signal in the data, so a page of names that start with
  "telekom" can still outrank "Slovak Telekom".

### Dictionary fuzzy (fuzzy v2)

- **How it works:**
  1. A query word that is a known word prefix stays as it is.
  2. An unknown word is corrected against `search_word` (trigram `%` on 360 k short words, top 60),
     then re-ranked by edit distance with adjacent transpositions. At most 1 typo is allowed in
     words of up to 4 letters and 2 above. All alternatives at the best distance are kept.
  3. The corrected words run through the normal word search.
- **Index:** 10 MB of trigram GIN on the dictionary, instead of 43 MB on full names.

| 12 typo queries (full machine) | p50 / p95 | Correct subject in top 3 |
| --- | --- | --- |
| fuzzy v1 (trigram over full names + rerank) | 9.5 / 59 ms | 10 |
| **fuzzy v2 (dictionary)** | **3.7 / 5 ms** | **11** |

- "slovanft" is now found: it is corrected to "slovnaft" or "slovanet", and Slovnaft is second.
- **The miss:** "esset" is a valid prefix of some other word, so no correction is attempted.

### Concurrent load (closed loop, no think time, no debounce: a stress test)

- **The test:** each worker types a random query character by character (every keystroke from the
  third is one default v2 search), then looks up the first hit. 5 % of sessions also run one
  dictionary-fuzzy search. `statement_timeout` is 5 s.
- **Tiers:** Docker limits on the same data, as before.
- **Runs:** each 45 s.
  - "sync" means a concurrent loop of the daily RPO files and a forced full re-merge of
    `ds_dsrdp` (1.42 M rows). That re-merge took 2 min on the PS-20-like tier and 5.6 min on the
    PS-10-like tier while the load ran.
  - The first run of each tier started with partly cold caches.

| Tier, run | Searches/s (throughput) | Search p50 / p95 | Lookup p95 |
| --- | --- | --- | --- |
| PS-20-like, 10 workers | 89 | 93 / 395 ms | 205 ms |
| PS-20-like, 10 workers + sync | 138 | 5 / 293 ms | 190 ms |
| PS-20-like, 20 workers | 75 | 106 / 999 ms | 406 ms |
| PS-10-like, 10 workers | 28 | 191 / 1 303 ms | 799 ms |
| PS-10-like, 10 workers + sync | 33 | 106 / 1 105 ms | 597 ms |
| PS-10-like, 20 workers | 28 | 203 / 5 092 ms | 1 691 ms |

- **What it means:**
  - **Saturated throughput:** about 75–140 default searches a second on 1/4 vCPU, and about 30 on
    1/8 vCPU.
  - **Latency under saturation is queueing**, roughly workers ÷ throughput. It is not the cost of
    one search: alone, a search takes 1–5 ms.
  - **Capacity, as a rough estimate:** a person typing with a 120 ms client debounce sends about
    1–3 searches a second. That gives about 30–90 people typing at the same moment on 1/4 vCPU,
    and about 10–30 on 1/8 vCPU.
  - **A concurrent sync did not stop service.** Searches and lookups were served throughout, the
    sync finished, and nothing failed.
- **Not measured:** an open-loop test at a fixed arrival rate, which is a better model of real
  users; a real managed tier.

### Search proposal after the third pass (for the maintainers)

- **Default = v2 adaptive** (name prefix + word prefixes, word step bounded).
- **`fuzzy` = dictionary fuzzy.** It is explicit, never automatic, behind `REGISTER_SEARCH_FUZZY`
  (default `true`). It has a per-key rate limit, a global limit on concurrent fuzzy queries,
  `statement_timeout`, and answers `429` without queueing; all of these are configurable. When it
  is disabled the API returns `fuzzy_disabled`, and `GET /v1/capabilities` says so.
- **Indexes:** B-tree everywhere, plus a trigram GIN on the word dictionary only (10 MB). Full-name
  trigram GIN is no longer needed. Migrations are the same for every deployment.
- **Clients debounce** (about 120 ms) and never auto-select the first hit.
- **IČO check digit:** **778 real IČOs in RPO fail the mod-11 check (0.036 %), 16 of them
  active.** So the check is a warning, never a block. An IČO that fails it but exists in RPO is
  returned. The DIČ check is not used until a reliable source confirms it.
