# Data sources and attribution

This repository contains **no register data**. A running instance downloads the data from the
sources below. The data keeps its sources' licences, and the API includes the attribution text in
every response.

| Source | Publisher | Licence | Attribution |
| --- | --- | --- | --- |
| Register of Legal Entities (RPO) | Ministry of the Interior of the Slovak Republic | [CC BY 4.0](https://creativecommons.org/licenses/by/4.0/) ([source](https://rpo.minv.sk/rpo-api-doc.html)) | "Register právnických osôb, MV SR, CC BY 4.0" |
| Lists of the Financial Administration | Finančná správa SR | CC0 per the [exports page](https://www.financnasprava.sk/sk/elektronicke-sluzby/verejne-sluzby/zoznamy/exporty-z-online-informacnych); the portal [terms](https://opendata.financnasprava.sk/page/licence) ask for attribution | "Zdroj údajov: Finančná správa SR" |

The Register of Financial Statements (RÚZ, CC0) is not used in v1 (decision D16).

**Never ingested:**
- the Financial Administration IBAN lists (`ds_dph_iban`, `ds_dph_oud`): CC BY-NC-ND, no commercial
  use;
- VIES data: its terms forbid mirroring.

## Personal data

Most records in these registers concern natural persons, sole traders in particular. Their business
name and place of business are personal data, even though the law publishes them.

When you run an instance, **you** are the controller of the copy and decide its purpose. Before
offering name search to others, check your legal basis, retention and objection handling. The
register stores only what identifies a business: no residence (when it differs from the place of
business), no birth data, no representatives. It also supports suppressing a person from name
search on objection.
