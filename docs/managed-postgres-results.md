# Managed PostgreSQL test (OQ-21)

**Run on 2026-11-11** on PlanetScale Postgres with the real data. This page records what was
measured and how. It answers the throughput part of OQ-21; the open parts are listed at the end.

## Setup

- **Database:** PlanetScale Postgres 18.6, single node, AWS `eu-central-1` (Frankfurt), arm64,
  network-attached storage. Sizes, changed online during the test:

  | Size | vCPU | RAM | `shared_buffers` | `max_connections` |
  | --- | --- | --- | --- | --- |
  | PS-20 | 1/4 | 2 GiB | 335 MB | 25 |
  | PS-10 | 1/8 | 1 GiB | 159 MB | 25 |
  | PS-5 | 1/16 | 512 MiB | 67 MB | 25 |

- **Register:** the production image from the `Dockerfile`, on a laptop in Central Europe. `serve`
  was limited to 1 CPU and 512 MB. The Docker VM had 4 vCPUs.
- **Network:** about 20 ms per round trip from the API to the database (`SELECT 1` through
  PgBouncer, measured ten times). Every latency below includes this. An API in the same region
  as the database will answer faster; the throughput limits do not depend on it.
- **Connections:** `sync`, `migrate` and the operator on the direct port 5432; `serve` through
  PgBouncer on port 6432. TLS with `sslmode=verify-full&sslrootcert=system`.
- **Role:** one user-defined role that inherits `postgres`, plus `pg_read_all_stats`.
- **`pg_strict`:** enabled, with `require_where_on_update` and `require_where_on_delete` both `on`.
- **Load:** open loop (requests are sent at a fixed rate whatever the latency), 30 s warm-up,
  then 60 s per step. The queries were built from 400 active subjects sampled from the database
  at the start: the full name, the first word, two words, a prefix of the first word, and an IČO
  typed into the box, in equal parts. Lookups used the IČOs of the same subjects. Fuzzy search ran
  100 typo queries one at a time, as its production limit allows one per instance. The per-key
  rate limits were raised so the client was not what was measured. The sample stayed on the test
  machine and was deleted afterwards.

## Compatibility

| Check | Result |
| --- | --- |
| `migrate`, including `CREATE EXTENSION pg_trgm` | Passed |
| `pg_strict` refuses `DELETE` without `WHERE` | Yes; a full bootstrap, daily files and the FS lists then ran with no `pg_strict` error |
| Advisory lock on the direct connection | Taken; a second `sync` logged "another sync is running" and exited 0 |
| `serve` through PgBouncer with pgx defaults (prepared statements) | Works, no extra settings |
| Search, lookup, `/v1/sources`, MCP `tools/call` | `200`, full addresses on the search cards |
| `pg_stat_statements` | Not loaded by default ("must be loaded via shared_preload_libraries"); enable it in the provider's extension settings to get statement statistics |

## First run (PS-20)

| | Measured |
| --- | --- |
| Whole first run, RPO and the six FS lists | **6 min 52 s** |
| RPO: snapshot of 2026-11-07 (23 parts, 2 294 939 records) and daily files to 11-11, published | under 5 min |
| Peak memory of the `sync` container | 27.5 MiB |
| A second run straight after | 2 s, nothing applied |
| Database size after the first run | 2 230 MB |

The FS servers answered fast that day. On the previous day the same lists took about 15 minutes
each to download, so the length of a first run depends mostly on the sources.

| Table | Total | Heap | Rows (estimate) |
| --- | --- | --- | --- |
| `rpo_record` | 968 MB | 814 MB | 2 294 598 |
| `fs_tax_subject` | 367 MB | 275 MB | 1 423 473 |
| `search_token` | 311 MB | 108 MB | 2 382 323 |
| `search_name` | 207 MB | 117 MB | 1 024 414 |
| `fs_list_entry` | 206 MB | 130 MB | 648 238 |
| `fs_vat_payer` | 105 MB | 71 MB | 319 314 |
| `search_word` | 55 MB | 15 MB | 360 082 |

## Search capacity

Default search, requests per second offered → answered with `200` per second, and latency in ms.

**PS-20, connection pool 4 (the default here, see finding 1):**

| Offered | Answered | p50 | p95 | Other answers |
| --- | --- | --- | --- | --- |
| 20 | 19.9 | 148 | 198 | — |
| 30 | 26.6 | 2 790 | 5 440 | 119 × `503` |
| 45 | 10.8 | 6 030 | 7 676 | 1 994 × `503` |
| 60 | 3.7 | 6 429 | 8 559 | mostly `503` |

**PS-20, `pool_max_conns=20`:**

| Offered | Answered | p50 | p95 |
| --- | --- | --- | --- |
| 20 | 19.9 | 150 | 200 |
| 45 | 44.9 | 152 | 206 |
| 60 | 59.8 | 152 | 215 |
| 80 | 79.8 | 150 | 207 |
| 100 | 99.6 | 164 | 1 624 |
| 140 | 105.1 | 4 476 | 5 986 (1 727 × `503`) |

The database CPU reached 100 % on the provider's dashboard during the 100 and 140 steps.

**PS-10, `pool_max_conns=20`:**

| Offered | Answered | p50 | p95 |
| --- | --- | --- | --- |
| 20 | 20.0 | 150 | 203 |
| 45 | 44.9 | 154 | 207 |
| 60 | 59.9 | 151 | 206 |
| 80 | 79.7 | 152 | 210 |
| 100 | 99.7 | 169 | 2 076 |
| 140 | 86.0 | 4 725 | 7 066 (2 800 × `503`) |

**PS-10 sustained:** 60 searches a second for 10 minutes: 35 962 of 35 962 answered, p50 150 ms,
p95 206 ms, p99 244 ms. Latency did not drift, but the database CPU stayed near 100 % for the
whole run, so 60 a second is the ceiling of PS-10, not a working point.

**Pool size, PS-20, 30 s per step:**

| `pool_max_conns` | Ceiling (answered/s) | Bound by | At 120/s offered |
| --- | --- | --- | --- |
| 4 | about 27 | the pool | 1.1 answered/s |
| 8 | about 55 | the pool | 2.2 answered/s |
| 12 | about 80 | the pool | 59 answered/s |
| 20 | about 105–111 | database CPU | 111 answered/s |

**PS-5, `pool_max_conns=20`, right after the resize (cache emptied):**

| Run | Answered | p50 | p95 | Other answers |
| --- | --- | --- | --- | --- |
| 400 searches one at a time, cold | 399 | 170 | 318 | 1 × `503` |
| the same 400 again, warm | 400 | 148 | 192 | — |
| steps of 60 s, 5 to 80 a second | all | 145–159 | 197–309 | — |
| 10 a second for 5 minutes | 2 998 of 2 998 | 155 | 229 | — |
| 20 a second for 5 minutes | 5 999 of 5 999 | 148 | 213 | — |
| 30 a second for 5 minutes | 8 998 of 8 998 | 148 | 201 | — |

The latency looks as good as on the larger sizes, but the provider's dashboard showed the CPU and the
memory at 100 % already during the 10-a-second run: the small cluster runs on borrowed CPU and
has no room left for a burst or for the monthly reconcile.

**Search during a heavy sync (PS-5):** a full first run into a second database on the same cluster,
while search ran at 10 a second, reported per minute. The first run took 875 s (412 s alone on
PS-20). While it applied the RPO snapshot and built the search layer, search p95 rose from about
0.2 s to 0.7–2.8 s, and 40 of 9 000 searches (0.4 %) hit the query timeout (`503 timeout`). This is
a worst case: a monthly reconcile writes far less than a first run.

## Lookup and fuzzy

| | PS-20, pool 20 | PS-10, pool 20 |
| --- | --- | --- |
| Lookup by IČO, 80/s | p50 128 ms, p95 144 ms | p50 128 ms, p95 157 ms |
| Lookup by IČO, 140/s | 139.4 answered, p95 166 ms | 139.4 answered, p95 423 ms |
| Fuzzy, 100 one at a time | p50 278 ms, p95 355 ms | p50 284 ms, p95 423 ms |

On PS-5, lookup held 60 a second (p95 145 ms) and broke at 80 (p95 3.3 s, 17 × `503`); fuzzy search
answered in p50 374 ms, p95 492 ms.

Lookup is an index read and did not reach the database limit. Fuzzy search was faster than the
Docker estimate of the spike (0.5–1 s on 1/4 vCPU).

## Findings

1. **The API pool sets the ceiling when the database is far away.** pgxpool opens at most
   `max(4, number of CPUs)` connections, and inside a container that is the CPUs the container
   sees, not its CPU quota (4 here). One search runs several queries in sequence, about 150 ms in
   total at 20 ms per round trip, so 4 connections give about 4 / 0.15 ≈ 27 searches a second.
   The pool sweep confirms it, including an explicit `pool_max_conns=4`. Locally, with almost no
   network latency, this was invisible. The pool size can be set without code changes with
   `pool_max_conns` in `REGISTER_DATABASE_URL`.
2. **When the pool is the bottleneck, overload collapses throughput.** At 45 searches a second
   offered with a pool of 4, only 2.8 a second were answered, instead of staying near 27 and
   refusing the rest. Requests wait for a connection for most of their timeout, then do part of
   the work and are cancelled. When the database CPU is the bottleneck (pool 20), the service holds
   its limit and refuses only the excess. Fixed by D39 and D40; see "After the fix" below.
3. **Database limits.** PS-20: about 100–110 default searches a second. PS-10: about 85, and 60 a
   second for 10 minutes at full CPU. PS-5: short runs up to 80 a second, but CPU and memory at
   100 % already at 10 a second sustained. With half of the measured limit as headroom for peaks,
   the monthly reconcile and growth: plan for up to about 30 a second on PS-10 and 50 on PS-20;
   PS-5 is for trying the register and for very light use, not for production.
4. **PgBouncer multiplexes.** With 21 client connections from the API, PgBouncer used at most 14–16
   server connections, within the 25 the small sizes allow.
5. **Cache.** 99.26 % of block reads were served from memory during the PS-20 runs. That says the
   working set of this test fits; it does not say that the whole database fits in memory.
6. **Resizing is online.** No resize needed a new database. PS-10 → PS-5 was watched from the
   client: a search every 2–3 seconds through the API and a direct query every 2–3 seconds got
   no error at all; one search at the cut-over took 2.2 s instead of about 0.16 s. The whole
   resize took a few minutes in the dashboard, but clients saw a pause of seconds. (PS-20 →
   PS-10 was not watched from the client; the provider's replication-lag graph showed about 1.5
   minutes while the new node caught up.)

## After the fix (D39, D40)

The same open-loop search steps, 30 s each, on PS-5 with the build that admits at most as many
requests as the pool has connections and refuses the rest after 250 ms with `503 overloaded`:

| Offered | Pool 4, before the fix (PS-20) | Pool 4, after | Pool 10 (the new default), after |
| --- | --- | --- | --- |
| 30 | 26.6 answered, p95 5 440 ms | 28.0 answered, p95 415 ms | 29.7 answered, p95 212 ms |
| 45 | 10.8 answered, p95 7 676 ms | 27.5 answered, p95 427 ms | 44.8 answered, p95 300 ms |
| 60 | 3.7 answered, p95 8 559 ms | 27.6 answered, p95 424 ms | 43.2 answered, p95 655 ms |
| 80 | 1.4 answered, timeouts | 27.5 answered, p95 423 ms | 51.5 answered, p95 617 ms |

An overloaded instance now keeps answering at its capacity, refuses the excess at once, and no
admitted request times out. With pool 10, PS-5 itself became the limit at about 45–50 a second.
The same run found that a `serve` started on a database without the newest migration answered
every request with `503 database`; `serve` now refuses to start on an older schema and says to run
`migrate`.

## Not answered yet

- **Latency with the API in the same region as the database.** The numbers above include about
  20 ms per database round trip.
- **Search during the monthly reconcile** on a production size (OQ-40). The worst case, a full
  first run on PS-5, is measured above.
