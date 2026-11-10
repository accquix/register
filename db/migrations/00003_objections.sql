-- Objections as records of their own, and the installation's setup state.
--
-- search_suppression held keys, and (since 00002) copied an objection onto every key a record ever
-- carried. One wrong IČO in one daily file then hid an unrelated subject for good, and undoing that
-- removed the person's objection with it. The model is now:
--
--   objection         what the person asked for: one row per objection, with its own id, the key
--                     the operator named (an IČO or one RPO record), when, and when it was revoked.
--                     A revoked row stays, as the record of the undo.
--   objection_record  the RPO records an objection is anchored to: those that carried its IČO when
--                     it was recorded, and those later created with that IČO. An anchored record
--                     stays hidden when it loses or changes its IČO.
--
-- What is hidden is computed from these and the current RPO data, per record:
--   a record is hidden when an active objection names its current IČO, names the record itself,
--   or is anchored to it.
-- Nothing is copied to other keys, so a wrong IČO never hides the subject that really owns it, and
-- a correction in RPO needs no clean-up here.
--
-- register_setup records that the operator confirmed the objections of this installation: either
-- "new" (there are none to restore) or "restored" (a backup was put back). Until then the API does
-- not serve: an empty database cannot tell a first installation from a lost one.

-- +goose Up

CREATE TABLE objection (
    id         bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    ico        text        CHECK (ico ~ '^[0-9]{8}$'),
    rpo_id     bigint      CHECK (rpo_id > 0),
    reason     text,                                            -- operator note, never personal data
    created_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz,
    CHECK ((ico IS NULL) <> (rpo_id IS NULL))
);
CREATE INDEX objection_ico ON objection (ico) WHERE ico IS NOT NULL AND revoked_at IS NULL;
CREATE INDEX objection_rpo_id ON objection (rpo_id) WHERE rpo_id IS NOT NULL AND revoked_at IS NULL;

CREATE TABLE objection_record (
    objection_id bigint NOT NULL REFERENCES objection (id),
    rpo_id       bigint NOT NULL,
    PRIMARY KEY (objection_id, rpo_id)
);
CREATE INDEX objection_record_rpo_id ON objection_record (rpo_id);

-- What the API may see: which keys are suppressed now. Not the reasons, not the history.
CREATE VIEW objection_active AS
    SELECT ico, rpo_id FROM objection WHERE revoked_at IS NULL;

-- Carry the existing objections over. Only the operator's own rows become objections; rows that
-- 00002 propagated onto record keys become anchors of their objection; rows it propagated onto
-- another IČO are dropped (that copying is the defect this migration removes).
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM search_suppression WHERE skey !~ '^([0-9]{8}|r[0-9]{1,18})$') THEN
        RAISE EXCEPTION 'search_suppression holds a key that is neither an IČO nor r<RPO id>; fix it by hand before migrating';
    END IF;
END
$$;
-- +goose StatementEnd

INSERT INTO objection (ico, rpo_id, reason, created_at)
SELECT CASE WHEN skey ~ '^[0-9]{8}$' THEN skey END,
       CASE WHEN skey ~ '^r' THEN substr(skey, 2)::bigint END,
       reason, created_at
FROM search_suppression WHERE origin IS NULL
ORDER BY created_at, skey;

INSERT INTO objection_record (objection_id, rpo_id)
SELECT o.id, substr(s.skey, 2)::bigint
FROM search_suppression s JOIN objection o ON o.ico = s.origin
WHERE s.skey ~ '^r'
ON CONFLICT DO NOTHING;

-- Anchor every objection by IČO to the records that carry the IČO now, at once: an objection
-- recorded before this migration must hold from the first unit applied after it.
INSERT INTO objection_record (objection_id, rpo_id)
SELECT o.id, r.id FROM objection o JOIN rpo_record r ON r.ico = o.ico
ON CONFLICT DO NOTHING;

DROP TABLE search_suppression;

CREATE TABLE register_setup (
    only_row                boolean     PRIMARY KEY DEFAULT true CHECK (only_row),
    objections_confirmed_at timestamptz,
    objections_confirmed_as text        CHECK (objections_confirmed_as IN ('new', 'restored', 'existing')),
    CHECK ((objections_confirmed_at IS NULL) = (objections_confirmed_as IS NULL))
);
-- A database that already published data keeps serving: its objections are the ones in place.
INSERT INTO register_setup (objections_confirmed_at, objections_confirmed_as)
SELECT CASE WHEN p THEN now() END, CASE WHEN p THEN 'existing' END
FROM (SELECT EXISTS (SELECT 1 FROM sync_source WHERE published_as_of IS NOT NULL) AS p) x;

-- +goose Down

CREATE TABLE search_suppression (
    skey       text        PRIMARY KEY,
    reason     text,
    created_at timestamptz NOT NULL DEFAULT now(),
    origin     text
);
CREATE INDEX search_suppression_origin ON search_suppression (origin) WHERE origin IS NOT NULL;
INSERT INTO search_suppression (skey, reason, created_at)
SELECT DISTINCT ON (k) k, reason, created_at
FROM (SELECT coalesce(ico, 'r' || rpo_id) AS k, reason, created_at FROM objection WHERE revoked_at IS NULL) x
ORDER BY k, created_at;

DROP TABLE register_setup;
DROP VIEW objection_active;
DROP TABLE objection_record;
DROP TABLE objection;
