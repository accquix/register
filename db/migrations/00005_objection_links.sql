-- Links between objections and RPO records get a provenance and can be released.
--
-- Two requirements pull against each other when the source data is wrong:
--   1. a person who objected must not come back into name search without anyone deciding so;
--   2. another subject must not stay hidden for good because of a wrong IČO in a source file.
-- No matching rule can tell a source mistake from a real change, so the register does not guess:
-- it errs towards hiding, records why, and lets the operator correct one link.
--
--   objection          the person's request (unchanged)
--   objection_record   a link: this objection covers this RPO record. Created by the operator's
--                      command for the records that carry the IČO then ("at_objection"), and by
--                      the sync for every record seen carrying the IČO later ("carried_ico"),
--                      with the unit that showed it. A link stays when the record loses or
--                      changes its IČO. It never extends to other records with the new IČO.
--   released_at        the operator's exclusion: this record does not belong to the objection.
--                      The row stays, so the next sync does not link the record again, until
--                      the operator withdraws the release.
--
-- A record is hidden while an active objection names it, has an unreleased link to it, or names
-- the IČO it carries now and the pair is not released.

-- +goose Up

ALTER TABLE objection_record
    ADD COLUMN basis           text        NOT NULL DEFAULT 'migrated'
        CHECK (basis IN ('at_objection', 'carried_ico', 'restored', 'migrated')),
    ADD COLUMN source          text,                             -- the sync unit, for 'carried_ico'
    ADD COLUMN created_at      timestamptz NOT NULL DEFAULT now(),
    ADD COLUMN released_at     timestamptz,
    ADD COLUMN released_reason text;                             -- operator note, never personal data
ALTER TABLE objection_record ALTER COLUMN basis DROP DEFAULT;

-- The earlier rule linked only records created with the IČO. Link every record that carries an
-- objected IČO now, at once, so the invariant holds from the first unit after this migration.
INSERT INTO objection_record (objection_id, rpo_id, basis)
SELECT o.id, r.id, 'migrated' FROM objection o JOIN rpo_record r ON r.ico = o.ico
WHERE o.revoked_at IS NULL
ON CONFLICT DO NOTHING;

-- What the API may see. An objection with a released record no longer hides its whole IČO at
-- query time (the search layer decides per record); it is left out of the by-IČO filter.
CREATE OR REPLACE VIEW objection_active AS
    SELECT CASE WHEN NOT EXISTS (SELECT 1 FROM objection_record a
                                 WHERE a.objection_id = o.id AND a.released_at IS NOT NULL)
                THEN o.ico END AS ico,
           o.rpo_id
    FROM objection o WHERE o.revoked_at IS NULL;

-- +goose Down

CREATE OR REPLACE VIEW objection_active AS
    SELECT ico, rpo_id FROM objection WHERE revoked_at IS NULL;
ALTER TABLE objection_record
    DROP COLUMN basis, DROP COLUMN source, DROP COLUMN created_at, DROP COLUMN released_at, DROP COLUMN released_reason;
