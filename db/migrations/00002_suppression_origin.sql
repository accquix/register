-- Objections follow the subject when its key changes.
--
-- A subject key is the valid IČO, else 'r' || RPO id, so it changes when a record gains or loses an
-- IČO. The sync therefore keeps search_suppression closed over both forms: a suppressed IČO also
-- suppresses every RPO record that carries it, and a suppressed record also suppresses the IČO it
-- carries. Rows added that way remember the key the operator gave in "origin", so that an undo
-- removes them together. origin is NULL for the operator's own row.

-- +goose Up

ALTER TABLE search_suppression ADD COLUMN origin text;
CREATE INDEX search_suppression_origin ON search_suppression (origin) WHERE origin IS NOT NULL;

-- +goose Down

DROP INDEX search_suppression_origin;
ALTER TABLE search_suppression DROP COLUMN origin;
