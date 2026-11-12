-- Taxpayer name search behind a switch, and objections by DIČ (D56).
--
-- A taxpayer without an IČO (a DIČ-only row of ds_dsrdp, mostly a private person) is found by name
-- only when the operator turns REGISTER_SEARCH_TAXPAYERS on, and only by the full name: every word
-- of the name and no other, in any order. register_name_key gives that order-free form of a
-- folded name (name_core): its words sorted by byte. The Go side builds the same key from the
-- query (internal/search), so no normalisation runs in SQL.
--
-- Such a person can object like any other: an objection by DIČ keeps the row out of taxpayer name
-- search. Exact lookup by DIČ is not affected (D26). There is no search layer for taxpayers: the
-- API checks objection_active at query time, so nothing has to be rebuilt.

-- +goose Up

-- +goose StatementBegin
CREATE FUNCTION register_name_key(core text) RETURNS text
LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE
AS $$
    SELECT array_to_string(ARRAY(
        SELECT w FROM unnest(string_to_array(core, ' ')) AS w WHERE w <> '' ORDER BY w COLLATE "C"), ' ')
$$;
-- +goose StatementEnd

CREATE INDEX fs_tax_subject_name_key ON fs_tax_subject (register_name_key(name_core))
    WHERE ico IS NULL AND absent_since IS NULL;

ALTER TABLE objection ADD COLUMN dic text CHECK (dic ~ '^[0-9]{10}$');
ALTER TABLE objection DROP CONSTRAINT objection_check;
ALTER TABLE objection ADD CONSTRAINT objection_one_key CHECK (num_nonnulls(ico, rpo_id, dic) = 1);
CREATE INDEX objection_dic ON objection (dic) WHERE dic IS NOT NULL AND revoked_at IS NULL;

-- A person who objected by DIČ and later appears in RPO stays out of name search without anyone
-- acting: when the DIČ list gives the objected DIČ an IČO, the sync links the RPO records that carry
-- that IČO, with the basis 'dic_list'. Like every link it stays when the record or the DIČ list
-- changes later, and the operator can release it (D56, I13).
ALTER TABLE objection_record DROP CONSTRAINT objection_record_basis_check;
ALTER TABLE objection_record ADD CONSTRAINT objection_record_basis_check
    CHECK (basis IN ('at_objection', 'carried_ico', 'restored', 'migrated', 'dic_list'));

-- What the API may see, now with the objected DIČs, and the objected IČO also when a link of the
-- objection is released (ico_any): the taxpayer search hides DIČ rows by the names of objected
-- subjects, and a released record does not end the objection for the rest of the subject.
CREATE OR REPLACE VIEW objection_active AS
    SELECT CASE WHEN NOT EXISTS (SELECT 1 FROM objection_record a
                                 WHERE a.objection_id = o.id AND a.released_at IS NOT NULL)
                THEN o.ico END AS ico,
           o.rpo_id,
           o.dic,
           o.ico AS ico_any
    FROM objection o WHERE o.revoked_at IS NULL;

-- The records active objections cover through an unreleased link: ids only, no reasons, no
-- history. The taxpayer search reads their names (a record keeps its link when it loses the IČO).
CREATE VIEW objection_link AS
    SELECT a.rpo_id FROM objection_record a JOIN objection o ON o.id = a.objection_id
    WHERE o.revoked_at IS NULL AND a.released_at IS NULL;

-- +goose Down

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM objection WHERE dic IS NOT NULL) THEN
        RAISE EXCEPTION 'objections by DIČ exist; they cannot be kept by the older schema. Export them first (suppress --export)';
    END IF;
    IF EXISTS (SELECT 1 FROM objection_record WHERE basis = 'dic_list') THEN
        RAISE EXCEPTION 'links of objections by DIČ exist; they cannot be kept by the older schema';
    END IF;
END
$$;
-- +goose StatementEnd

DROP VIEW objection_link;
DROP VIEW objection_active;
ALTER TABLE objection_record DROP CONSTRAINT objection_record_basis_check;
ALTER TABLE objection_record ADD CONSTRAINT objection_record_basis_check
    CHECK (basis IN ('at_objection', 'carried_ico', 'restored', 'migrated'));
CREATE VIEW objection_active AS
    SELECT CASE WHEN NOT EXISTS (SELECT 1 FROM objection_record a
                                 WHERE a.objection_id = o.id AND a.released_at IS NOT NULL)
                THEN o.ico END AS ico,
           o.rpo_id
    FROM objection o WHERE o.revoked_at IS NULL;
DROP INDEX objection_dic;
ALTER TABLE objection DROP CONSTRAINT objection_one_key;
ALTER TABLE objection DROP COLUMN dic;
ALTER TABLE objection ADD CONSTRAINT objection_check CHECK ((ico IS NULL) <> (rpo_id IS NULL));
DROP INDEX fs_tax_subject_name_key;
DROP FUNCTION register_name_key(text);
