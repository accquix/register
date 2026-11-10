# Sources

Everything here was checked live on **2026-11-10**. Re-check before relying on it, because state
endpoints change without notice. The RPO move in June 2026 broke many integrations.

## Overview

| Source | What we take | Access | Licence | Refresh |
| --- | --- | --- | --- | --- |
| **RPO**: Register of Legal Entities, MV SR since 2026-06-01 | IČO, name, legal form, address or place of business, establishment and termination, **register entry** (`sourceRegister`: register, office, number) | Public S3-compatible bucket, no key | **CC BY 4.0**, attribution required ([doc](https://rpo.minv.sk/rpo-api-doc.html)) | Monthly snapshot plus daily change files |
| **FS list ZIPs**: Financial Administration | DIČ (`ds_dsrdp`), IČ DPH and VAT paragraph (`ds_dphs`), risk lists (`ds_dphz`, `ds_dphv`, `ds_dpho`), reliability index (`ds_iz_ran`) | `https://report.financnasprava.sk/{slug}.zip`, no key, ETag/304 | **CC0** per the [exports page](https://www.financnasprava.sk/sk/elektronicke-sluzby/verejne-sluzby/zoznamy/exporty-z-online-informacnych). The portal terms ask for "Zdroj údajov: Finančná správa SR" ([licence](https://opendata.financnasprava.sk/page/licence)), so we always attribute | Daily ("Aktualizácia: denne"). In practice `ds_iz_ran` changes less often |
| **FS Lists API** (optional) | Same data, one subject at a time: a fallback when a ZIP fails | `https://iz.opendata.financnasprava.sk/api`, needs a key | as above | Daily, so never newer than the ZIP |
| **RÚZ**: Register of Financial Statements, MF SR | Second source of DIČ and cancellation dates | REST, no key ([API](https://www.registeruz.sk/cruz-public/home/api)) | **CC0** | Incremental by `zmenene-od` |
| **ORSR**: Commercial Register, Ministry of Justice | Court, oddiel and vložka confirmation | `sluzby.orsr.sk/api/legal-person`, no key | **Not stated.** robots.txt disallows `/api` | **On demand only**, until the licence is known (OQ-4) |

**Not used:**
- **ŽRSR:** a bot check on every search. RPO already carries the trade-register entry.
- **VIES:** its terms forbid mirroring. Consumers query it live.
- **`ds_dph_iban` and `ds_dph_oud`:** CC BY-NC-ND, no commercial use.
- **Slovensko.Digital dumps:** their terms forbid altering the data. We go to the state sources
  directly.

## RPO bucket

- **Base:** `https://frkqbrydxwdp.compat.objectstorage.eu-frankfurt-1.oraclecloud.com/susr-rpo/`.
  Anonymous S3 listing works: `?list-type=2&prefix=batch-daily/`.
- **Snapshot:** `batch-init/init_YYYY-MM-DD_NNN.json.gz`, 23 parts, about 100 000 records each,
  951.7 MB gzipped, about 10 GB JSON. A manifest `init_YYYY-MM-DD_list.txt` lists the parts.
  - It is published monthly (first Saturday) and uploaded the next day around 21:00 UTC.
  - **The manifest is written before the last parts:** check that every listed part exists before
    starting.
  - On 2026-11-10 two snapshots were retained, 2026-10-03 and 2026-11-07.
- **Daily files:** `batch-daily/actual_YYYY-MM-DD.json.gz`, 50 KB–2.3 MB.
  - Each holds **complete current records** of the entities added or changed, not field deltas.
  - They can arrive a day late. Every calendar date from 2026-09-26 to 2026-11-10 had a file.
- **Retention: 45 days.** After a longer outage the chain cannot be rebuilt from daily files; reload
  from a snapshot.
- **Apply order** (doc): the newest snapshot, then the daily files dated **strictly after** it.
- **Layout:** line 1 is `{"exportDate":"YYYY-MM-DD","results":[`, then one record per line (later
  lines start with `,{`), and the last line is `]}`. Parse it as a token stream; do not split on
  lines.
- **Record keys:**
  - always: `id`, `identifiers` (IČO with `validFrom`), `fullNames`, `addresses`, `legalForms`
    (codelist CL000056), `establishment`, `statutoryBodies`, `sourceRegister`,
    `containsInconsistency`;
  - sometimes: `termination`, `legalStatuses`, `activities`, `stakeholders`, `statisticalCodes`…
- **Ends of entities:** an entity ends through `termination`. A physical-deletion marker has not
  been seen (UNVERIFIED).
- **IČO is not unique:** more than one RPO record can carry the same IČO.
- **Register entry examples** (`sourceRegister`):

  ```json
  {"value":{"codelistCode":"CL010112","code":"1","value":"Obchodný register"},
   "registrationOffices":[{"value":"Mestský súd Bratislava III","validFrom":"2017-06-22"}],
   "registrationNumbers":[{"value":"Sro/125914/B","validFrom":"2017-06-22"}]}
  {"value":{"codelistCode":"CL010112","code":"2","value":"Živnostenský register"},
   "registrationOffices":[{"value":"Okresný úrad Michalovce","validFrom":"2022-01-24"}],
   "registrationNumbers":[{"value":"840-29282","validFrom":"2022-01-24"}]}
  ```

  - These give "zapísaná v Obchodnom registri Mestského súdu Bratislava III, oddiel: Sro, vložka č.
    125914/B" and "zapísaný v Živnostenskom registri Okresného úradu Michalovce, č. 840-29282".
    Turning the court name into the genitive is the consumer's job.
  - RPO uses the post-2023 court names. ORSR's court code no longer matches the name, so take the
    text, never the code.
- **Sample of one daily file:** of 167 records, 130 came from ŽRSR and 30 from ORSR.

## FS ZIPs

- **Contents:** each ZIP holds the XML and an XSD, deflated. `ds_dphs.xml` is 127 MB, UTF-8, CRLF.
  - Root `ZoznamSubjektovRegistrovanychkDPH`, with `DatumAktualizacieZoznamu` (`ddmmyyyy`).
  - Rows are `DS_DPHS/ITEM`.
  - Other lists are presumed to follow the same pattern (UNVERIFIED).
- **`ds_dphs` fields:** `IC_DPH`, `ICO` (optional), `NAZOV_DS`, `OBEC`, `PSC`, `ULICA_CISLO`, `STAT`,
  `DRUH_REG_DPH` (§4, §4b, §5, §7, §7a), `DATUM_REG`, `DATUM_ZMENY_DRUHU_REG`, `PLAT_DPH_OD`. Dates
  are `dd.mm.yyyy`.
  - The FS page explains that `DATUM_REG` and `PLAT_DPH_OD` mean different things before and after
    1 Jan 2025
    ([FS](https://www.financnasprava.sk/sk/elektronicke-sluzby/verejne-sluzby/zoznamy/zoznam-registrovanych-pre-DPH)).
- **There is no clean key in `ds_dphs`:**
  - 11 446 of 319 173 rows have no ICO, and 26 have a non-8-digit ICO.
  - **124 IC_DPH values repeat:** these are §4b VAT groups, with up to 534 members.
  - Proposed key: `IC_DPH`, plus the ICO or the normalised name.
- **No cancellation date and no history exist anywhere.** A deregistered payer simply disappears
  from `ds_dphs`. `ds_dphv` lists removals from the §52(6) risk list, not VAT cancellations.
- **The files regenerate** around 01:36–01:52 UTC.
- **Do not download the ZIPs in a loop.** Use a conditional GET with the stored ETag.

## RÚZ (not in v1, decision D16; kept for v2)

- **Listing:** `zmenene-od` (inclusive, day or timestamp), paged with `pokracovat-za-id` and
  `max-zaznamov` (at most 10 000), with an `existujeDalsieId` flag. `zostavajuce-id/...` returns the
  remaining count.
- **Details:** one request per entity, `/api/uctovna-jednotka?id=`.
- **Deletions** come back as `{"stav":"ZMAZANÉ"}`.
- **Next watermark:** the doc says to use the date the download **started**.
- **First run:** 1 935 750 entity ids; "môže trvať aj viac dní" (it can take several days). At
  about 10 requests per second that is about 54 h (estimate). RÚZ tolerance for request rate is
  UNVERIFIED.

## Measured volumes (2026-11-10)

| Dataset | Rows | Raw size |
| --- | --- | --- |
| RPO snapshot 2026-11-07 | about 2.28 M records (≈1.1 M active); about 80 % from the Trade Register (sole traders) | 951.7 MB gz → about 10 GB JSON |
| RPO daily | 131–4 270 records a file, about 2 000 on average (measured, 43 files) | 50 KB – 2.3 MB gz |
| `ds_dsrdp` (DIČ) | 1 423 049 (11 % without IČO) | 56.8 MB zip → 399 MB XML |
| `ds_dphs` (VAT) | 319 173: §4 226 924, §7a 76 182, §5 9 437, §7 5 158, §4b 1 472 | 14.4 MB → 127 MB |
| `ds_iz_ran` | 648 238 | 24.6 MB → 200 MB |
| `ds_dphz` / `ds_dphv` / `ds_dpho` | 3 805 / 44 284 / 1 004 | small |
| RÚZ | 1 935 750 entities | about 0.7 GB JSON |

**Measured core database (spike, 2026-11-10): 2.1 GB in PostgreSQL 18, indexes included** (RPO and the FS lists, no RÚZ). See [spike-results.md](spike-results.md).
