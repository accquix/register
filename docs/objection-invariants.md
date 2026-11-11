# Objections: the contract

This is the fixed set of rules the objection ("suppress") model must keep. A change to the code is
checked against these rules; a review checks that they hold, not whether a different design would
be nicer. Changing a rule is a maintainers' decision ([decisions.md](decisions.md), D26, D31, D32,
D33, D42), not a code change. Each rule names the test that fails when it breaks.

**What it is for.** A natural person can ask to be left out of *name search*. Exact lookup by
identifier is not affected (D26). Whether an operator must grant a given request is a legal
question for the operator (D48); the register only provides the mechanism.

## Terms

- **Objection:** the person's request. A number, the key the operator named (an IČO, or one RPO
  record), when it was recorded, when it was revoked.
- **Link:** "this objection covers this RPO record", with its basis, the sync unit that caused it,
  and when.
- **Release:** the operator's exclusion of one link. The row stays.
- **Hidden:** a record whose names, words and dictionary entries are not in the search layer.

## Rules

| # | Rule | Test |
| --- | --- | --- |
| I1 | A record is hidden exactly while an active objection names it, has an unreleased link to it, or names the IČO it carries now and that pair is not released | `TestObjectionFollowsItsRecords`, `TestWrongICOIsContainedAndCorrectable` |
| I2 | **No record leaves an objection without the operator.** After any committed sync unit or operator command, every record that carries an objected IČO is linked or released. A linked record stays hidden when it loses or changes its IČO | `TestObjectionFollowsItsRecords`, `TestObjectionBeforeTheFirstImport` |
| I3 | **No spreading.** A link covers one record. It never hides other records that share the record's new IČO, and it never creates an objection | `TestWrongICOIsContainedAndCorrectable` |
| I4 | **Never silent.** Every link the sync adds is logged with the objection's number and the unit, and is listed by `suppress --list --id N` with its basis and source | `TestWrongICOIsContainedAndCorrectable` (source), sync log |
| I5 | **Correctable, and it stays corrected.** The operator can release one link without touching the objection or its other links. A released record is not linked or hidden by that objection again, also while it carries the IČO, until the operator withdraws the release | `TestWrongICOIsContainedAndCorrectable` |
| I6 | An undo revokes exactly one objection, by its number, and keeps the row. It never removes another objection, and it reports how many records are still hidden | `TestUndoKeepsIndependentObjections` |
| I7 | **Import and objections never disagree.** Wherever an objection lands inside a sync unit (after staging, after the merge, before the commit) and whichever commits first, the record is not searchable afterwards and is linked | `TestObjectionAtEveryStageOfAUnit`, `TestUnitWaitsForAnObjectionBeingRecorded` |
| I8 | Nothing of a hidden record is reachable through name search: not its names, not its words, not a fuzzy correction to a word only hidden records use. The search queries check the objected keys again at query time | API tests "objection keeps a subject out of name search only" and "an objection is enforced at query time" |
| I9 | **Fail closed after a rebuild.** The API does not serve until the operator restored a backup or confirmed a new installation with no objections | API test "nothing is served until the operator confirms the objections" |
| I10 | A restore only adds. It never removes, revokes or re-activates an objection, and it keeps links and releases. A backup carries revocations | `TestRestoreKeepsLinksAndReleases`, `TestRestoreNeverReactivatesARevokedObjection` |
| I11 | A backup file can only grow: an export never replaces a file that holds an objection the database does not have, and an unconfirmed installation cannot export | `TestExportNeverDestroysABackup` |
| I12 | Roles: the API sees which keys are objected, never reasons or history; the sync can link records but cannot add, change, revoke or release; the operator can add, revoke and release but cannot rewrite or delete an objection or write register data | `TestSeparateRoles` |

## Known limitations (accepted, documented)

- **A wrong IČO in a source file can hide one record of another subject** (I2 wins over
  convenience). The register cannot tell a source mistake from a real change. The effect is one
  record, visible in the log and the list, and removable with `suppress --release` (I3–I5).
- **An objection does not cover a person's records that never shared an IČO with it.** The
  operator names them (OQ-36).
- **A backup older than a revocation re-activates nothing in a database that still has the
  objection** (I10), but restoring it into an empty database restores the objection as it was when
  the backup was taken. Export after every change.
- **The checksum in a backup shows damage, not authorship.** Encryption and access control protect
  the file.
- **Any database role that can connect can take advisory locks** and so delay the sync or an
  operator command. Table rights cannot prevent it; protect the API's database credentials.
