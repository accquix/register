-- +goose Up

-- D41: a database loaded by `accquix-register demo` holds fictional subjects for good. The API
-- labels every answer as demo data, and `sync` refuses to write real data into it.
ALTER TABLE register_setup ADD COLUMN demo_since timestamptz;

-- +goose Down

ALTER TABLE register_setup DROP COLUMN demo_since;
