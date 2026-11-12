# Objections: the contract

This is the fixed set of rules the objection ("suppress") model must keep. A change to the code is
checked against these rules; a review checks that they hold, not whether a different design would
be nicer. Changing a rule is a maintainers' decision ([decisions.md](decisions.md), D26, D31, D32,
D33, D42, D56), not a code change. Each rule names the test that fails when it breaks.

**What it is for.** A natural person can ask to be left out of *name search*. Exact lookup by
identifier is not affected (D26). Whether an operator must grant a given request is a legal
question for the operator (D48); the register only provides the mechanism.

## Terms

- **Objection:** the person's request. A number, the key the operator named (an IČO, one RPO
  record, or the DIČ of a taxpayer without an IČO), when it was recorded, when it was revoked.
- **Link:** "this objection covers this RPO record", with its basis, the sync unit that caused it,
  and when. The basis `dic_list`: the DIČ list gave the objected DIČ the IČO the record carries.
- **Release:** the operator's exclusion of one link. The row stays.
- **Hidden:** a record whose names, words and dictionary entries are not in the search layer.

## Rules

| # | Rule | Test |
| --- | --- | --- |
| I1 | A record is hidden exactly while an active objection names it, has an unreleased link to it, or names the IČO it carries now and that pair is not released | `TestObjectionFollowsItsRecords`, `TestWrongICOIsContainedAndCorrectable` |
| I2 | **No record leaves an objection without the operator.** After any committed sync unit or operator command, every record that carries an objected IČO is linked or released. A linked record stays hidden when it loses or changes its IČO | `TestObjectionFollowsItsRecords`, `TestObjectionBeforeTheFirstImport` |
| I3 | **No spreading.** A link covers one record. It never hides other records that share the record's new IČO, and it never creates an objection | `TestWrongICOIsContainedAndCorrectable` |
| I4 | **Never silent.** Every link the sync adds is logged with the objection's number and the unit, and is listed by `suppress --list --id N` with its basis and source | `TestWrongICOIsContainedAndCorrectable` (source), sync log |
| I5 | **Correctable, and it stays corrected.** The operator can release one link without touching the objection or its other links. A released record is not linked or hidden by that objection again, also while it carries the IČO, until the operator withdraws the release. Only a link that exists can be released: a record the objection never covered is refused | `TestWrongICOIsContainedAndCorrectable`, `TestReleaseNeedsALink` |
| I6 | An undo revokes exactly one objection, by its number, and keeps the row. It never removes another objection, and it reports how many records are still hidden | `TestUndoKeepsIndependentObjections` |
| I7 | **Import and objections never disagree.** Wherever an objection lands inside a sync unit (after staging, after the merge, before the commit) and whichever commits first, the record is not searchable afterwards and is linked | `TestObjectionAtEveryStageOfAUnit`, `TestUnitWaitsForAnObjectionBeingRecorded` |
| I8 | Nothing of a hidden record is reachable through name search: not its names, not its words, not a fuzzy correction to a word only hidden records use. The search queries check the objected keys again at query time | API tests "objection keeps a subject out of name search only" and "an objection is enforced at query time" |
| I9 | **Fail closed after a rebuild.** The API does not serve until the operator restored a backup or confirmed a new installation with no objections | API test "nothing is served until the operator confirms the objections" |
| I10 | A restore only adds. An objection or a link that exists in the database is kept exactly as it is: a restore never removes, revokes or re-activates an objection, and never releases or re-links a record (the database is at least as new as any backup). Into a database that lacks them, it restores objections with their links, releases and revocations | `TestRestoreKeepsLinksAndReleases`, `TestRestoreNeverReactivatesARevokedObjection`, `TestRestoreNeverReleasesAnExistingLink` |
| I11 | A backup file can only grow: an export never replaces a file that holds an objection the database does not have, and an unconfirmed installation cannot export | `TestExportNeverDestroysABackup` |
| I12 | Roles: the API sees which keys are objected, never reasons or history; the sync can link records but cannot add, change, revoke or release; the operator can add an objection, set its revocation and release a link, but cannot change its key, reason or time, cannot delete it, and cannot write register data | `TestSeparateRoles` |
| I13 | **A taxpayer without an IČO objects by DIČ.** Such an objection has no links. While it is active, taxpayer name search never returns that DIČ; the check runs on every query, so no rebuild, restore or upgrade can open a gap. Exact lookup by DIČ is unaffected. A DIČ that belongs to a subject with an IČO is refused (object by the IČO). **When the DIČ list later gives the objected DIČ an IČO, the sync links the records that carry it** (basis `dic_list`), and from then on I1–I5 apply to those links: they stay when the IČO goes away again, and only the operator releases them; a released `dic_list` link shows the record again (no other check hides it). **An objection by IČO or record reaches the taxpayer search too:** when an objected subject's current name holds the query's full name as adjacent words, no taxpayer is returned for that query | `TestTaxpayerNameSearch` (objection, restore, MCP, the DIČ → IČO transition, namesakes), `TestContainsFullName`, `TestObjectionBackupRoundTrip` |

## Known limitations (accepted, documented)

- **A wrong IČO in a source file can hide one record of another subject** (I2 wins over
  convenience). The register cannot tell a source mistake from a real change. The effect is one
  record, visible in the log and the list, and removable with `suppress --release` (I3–I5).
- **An objection does not cover a person's records that never shared an IČO with it.** The
  operator names them (OQ-36).
- **A backup older than a revocation re-activates nothing in a database that still has the
  objection** (I10), but restoring it into an empty database restores the objection as it was when
  the backup was taken. Export after every change.
- **A wrong IČO in the DIČ list for an objected DIČ hides a whole subject, not one record.** The
  `dic_list` links cover every record that carries that IČO while the list gives it, each logged
  and listed with its unit; the operator releases them one by one (`suppress --release`), and a
  released record is never linked again by that objection. The DIČ list is the official link
  between a DIČ and an IČO, so the register follows it and errs towards hiding.
- **The taxpayer search matches an objected subject by its RPO names** (by the objected IČO even
  when a link of the objection is released, by a named record, and by every linked record). A DIČ row of the
  same person under a name the objected records never carried (a maiden name, a name the RPO
  record lost) is covered only by an objection by its DIČ. The name rule also hides namesakes of
  an objected person from taxpayer search: privacy wins over completeness.
- **The checksum in a backup shows damage, not authorship.** Encryption and access control protect
  the file.
- **The query-time check (I8) is by IČO and by directly named record.** For an objection with a
  released record it does not apply by IČO, because the released record shares that IČO and must
  stay findable: there the search layer alone decides, as it does for linked records that no
  longer carry the IČO.
- **The operator's right on an objection is its revocation time.** Table rights cannot say "only
  from empty to now", so an operator can also clear or change that time. Clearing it re-activates
  the objection (more is hidden, never less).
- **Any database role that can connect can take advisory locks** and so delay the sync or an
  operator command. Table rights cannot prevent it; protect the API's database credentials.
