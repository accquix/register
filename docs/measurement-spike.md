# Measurement spike (OQ-2)

**Status: done 2026-11-10.** Results: [spike-results.md](spike-results.md). The spike is throw-away measurement code, not the build. It runs
locally and costs nothing.

**Goal:** replace the estimates with measured numbers before choosing the database size and before
fixing the validation thresholds.

**Time-sensitive:** the RPO bucket keeps files for about 45 days. The 2026-10-03 snapshot will
probably disappear around **2026-11-17**. Running before then allows the one full chain test:
snapshot 2026-10-03 → daily files 10-04…11-07 → reconcile against snapshot 2026-11-07.

**Effort:** about half a day.
- 2–4 h writing the throw-away code (schema, RPO streaming loader, FS XML loader, searches).
- The rest is machine time.
- About 30 GB of free disk is needed.
- RÚZ is excluded; a small sample is enough.

## Steps

1. Run PostgreSQL 18 with `pg_trgm` in Docker, twice:
   - unconstrained;
   - constrained to `--cpus=0.25 --memory=2g`, a stand-in for a small managed
     tier (1/4 vCPU, 2 GiB).

   The loader runs with `--memory=1g`.
2. Bootstrap from `init_2026-10-03`. Per part, record wall time, rows per second, peak RSS and CPU.
3. Build the indexes. Record the time and the table and index sizes (`pg_total_relation_size`).
4. Apply the daily files 10-04…11-07 and record the time per file.
5. Reconcile against `init_2026-11-07` and count the rows that change: this is the
   daily-chain-completeness test.
6. Re-apply one daily file. Expect 0 updated rows.
7. Kill test: `kill -9` the loader in the middle of a part and in the middle of `ds_dsrdp`, then
   rerun. The table checksum must equal a clean run's.
8. Load all six FS lists, and check the 304 path.
9. Run 30 real searches with `EXPLAIN (ANALYZE, BUFFERS)`, cold and warm, and record p50 and p95.
10. Later, optionally: repeat steps 2–4 and 9 on a small paid managed PostgreSQL tier,
    over a direct connection.

## Pass thresholds (proposals)

| Check | Pass | Fail |
| --- | --- | --- |
| Loader peak RSS | ≤ 600 MB | > 900 MB |
| Bootstrap, local | ≤ 30 min | — |
| Bootstrap, constrained or paid tier | ≤ 4 h | > 8 h |
| One RPO daily file, constrained | ≤ 2 min | — |
| Full FS daily set, constrained | ≤ 15 min | — |
| Reconcile residual difference | ≤ 0.1 % of records | above: the daily chain is untrustworthy |
| Kill and rerun checksum | identical | any difference |
| Core database size | ≤ 5 GB | > 8 GB |
| Search p95, warm | ≤ 50 ms | > 200 ms |
| Search p95, cold | ≤ 150 ms | — |

**Output:** `docs/spike-results.md` with the numbers. Downloaded data stays outside the repository.
