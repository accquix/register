-- Initial schema (architecture v1, decisions D17–D30).
-- Plain PostgreSQL 16–18; the only extension is pg_trgm. No UPDATE/DELETE without WHERE anywhere
-- (PlanetScale pg_strict); staging tables are emptied with TRUNCATE.

-- +goose Up

CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- ---------------------------------------------------------------------------------------------
-- Source tables: one current-state table per source, keyed by the source's own key.
-- Common columns: source_as_of (date the agency stamped), row_hash (SHA-256 over the exposed
-- normalised columns), first_seen_at, changed_at, and an absence observation (never a legal date).
-- ---------------------------------------------------------------------------------------------

CREATE TABLE rpo_record (
    id                      bigint      PRIMARY KEY,           -- RPO internal id
    ico                     text        CHECK (ico ~ '^[0-9]{8}$'), -- null when the record has no valid IČO
    kind                    text        NOT NULL CHECK (kind IN ('legal_person', 'sole_trader', 'public_body', 'other')),
    name                    text,
    name_core               text        NOT NULL DEFAULT '',
    legal_suffix            text,
    legal_form_code         text,
    legal_form              text,
    established_on          date,
    terminated_on           date,
    street                  text,
    building_number         text,
    postal_code             text,
    municipality            text,
    country                 text,
    register_code           text,
    register_name           text,
    registration_office     text,
    registration_number     text,
    registration_valid_from date,
    source_as_of            date        NOT NULL,
    src_date                date        NOT NULL,               -- order guard: (src_date, src_rank)
    src_rank                smallint    NOT NULL,               -- 0 daily file, 1 snapshot
    row_hash                bytea       NOT NULL,
    first_seen_at           timestamptz NOT NULL DEFAULT now(),
    changed_at              timestamptz NOT NULL DEFAULT now(),
    removed_at              timestamptz
);
CREATE INDEX rpo_record_ico ON rpo_record (ico) WHERE ico IS NOT NULL;

CREATE TABLE fs_tax_subject (                                   -- ds_dsrdp
    dic           text        PRIMARY KEY CHECK (dic ~ '^[0-9]{10}$'),
    ico           text,
    name          text,
    name_core     text        NOT NULL DEFAULT '',
    municipality  text,
    postal_code   text,
    street        text,
    country       text,
    source_as_of  date        NOT NULL,
    row_hash      bytea       NOT NULL,
    first_seen_at timestamptz NOT NULL DEFAULT now(),
    changed_at    timestamptz NOT NULL DEFAULT now(),
    absent_since  timestamptz
);
CREATE INDEX fs_tax_subject_ico ON fs_tax_subject (ico) WHERE ico IS NOT NULL;

CREATE TABLE fs_vat_payer (                                     -- ds_dphs; VAT groups share an IČ DPH
    vkey            text        PRIMARY KEY,                    -- ic_dph '|' ico, or ic_dph '|n:' name_core
    ic_dph          text        NOT NULL CHECK (ic_dph ~ '^SK[0-9]{10}$'),
    ico             text,
    name            text,
    name_core       text        NOT NULL DEFAULT '',
    paragraph       text        NOT NULL,                       -- §4, §4b, §5, §7, §7a
    registered_on   date,
    type_changed_on date,
    payer_from      date,                                       -- always empty for §7 and §7a
    municipality    text,
    postal_code     text,
    street          text,
    source_as_of    date        NOT NULL,
    row_hash        bytea       NOT NULL,
    first_seen_at   timestamptz NOT NULL DEFAULT now(),
    changed_at      timestamptz NOT NULL DEFAULT now(),
    absent_since    timestamptz
);
CREATE INDEX fs_vat_payer_ic_dph ON fs_vat_payer (ic_dph);
CREATE INDEX fs_vat_payer_ico ON fs_vat_payer (ico) WHERE ico IS NOT NULL;

CREATE TABLE fs_list_entry (                                    -- ds_dphz, ds_dphv, ds_dpho, ds_iz_ran
    dataset       text        NOT NULL CHECK (dataset IN ('ds_dphz', 'ds_dphv', 'ds_dpho', 'ds_iz_ran')),
    subject_key   text        NOT NULL,
    ic_dph        text,
    dic           text,
    ico           text,
    name          text,
    attrs         jsonb       NOT NULL DEFAULT '{}'::jsonb,
    source_as_of  date        NOT NULL,
    row_hash      bytea       NOT NULL,
    first_seen_at timestamptz NOT NULL DEFAULT now(),
    changed_at    timestamptz NOT NULL DEFAULT now(),
    absent_since  timestamptz,
    PRIMARY KEY (dataset, subject_key)
);
CREATE INDEX fs_list_entry_ico ON fs_list_entry (ico) WHERE ico IS NOT NULL;
CREATE INDEX fs_list_entry_dic ON fs_list_entry (dic) WHERE dic IS NOT NULL;

-- Staging: emptied with TRUNCATE at the start of every unit. UNLOGGED: they hold nothing that must
-- survive a crash, because a crashed unit is rolled back and rerun.
CREATE UNLOGGED TABLE rpo_record_stage (LIKE rpo_record INCLUDING DEFAULTS);
CREATE UNLOGGED TABLE fs_stage (
    k            text,
    dataset      text,
    ic_dph       text,
    dic          text,
    ico          text,
    name         text,
    name_core    text,
    municipality text,
    postal_code  text,
    street       text,
    country      text,
    paragraph    text,
    d1           date,
    d2           date,
    d3           date,
    attrs        jsonb,
    source_as_of date,
    row_hash     bytea
);

-- ---------------------------------------------------------------------------------------------
-- Subject and search layer (D17, D18, D21, D29), maintained by sync in the same unit transaction.
-- skey: the valid IČO, else 'r' || RPO id. Only active, non-suppressed subjects are here.
-- ---------------------------------------------------------------------------------------------

-- Objections (GDPR): subjects kept out of name search. Keyed by subject key, so a new RPO record of
-- the same IČO stays suppressed. Exact identifier lookup is not affected.
CREATE TABLE search_suppression (
    skey       text        PRIMARY KEY,
    reason     text,                                            -- operator note, never personal data
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE search_name (                                      -- one row per distinct official name
    skey         text    NOT NULL,
    name_core    text    NOT NULL,
    name         text    NOT NULL,                              -- display form of this name
    ico          text,
    municipality text,
    kind         text    NOT NULL,
    is_primary   boolean NOT NULL DEFAULT false,                -- the subject's display name
    PRIMARY KEY (skey, name_core)
);
CREATE INDEX search_name_prefix ON search_name (name_core text_pattern_ops);

CREATE TABLE search_token (                                     -- distinct words (2+ chars) over all names
    skey  text NOT NULL,
    token text NOT NULL,
    PRIMARY KEY (skey, token)
);
CREATE INDEX search_token_prefix ON search_token (token text_pattern_ops, skey);

CREATE TABLE search_word (                                      -- word dictionary: adaptive step + fuzzy
    word text    PRIMARY KEY,
    freq integer NOT NULL CHECK (freq >= 0)                     -- number of subjects with this word
);
CREATE INDEX search_word_prefix ON search_word (word text_pattern_ops) INCLUDE (freq);
CREATE INDEX search_word_trgm ON search_word USING gin (word gin_trgm_ops);

-- ---------------------------------------------------------------------------------------------
-- Sync checkpoints and journal.
-- ---------------------------------------------------------------------------------------------

CREATE TABLE sync_source (
    source          text        PRIMARY KEY,                    -- 'rpo', 'fs:ds_dphs', ...
    state           text        NOT NULL DEFAULT 'empty'
                                CHECK (state IN ('empty', 'bootstrap', 'daily', 'gap_wait', 'reconcile', 'blocked')),
    base_snapshot   date,
    applied_through date,
    published_as_of date,
    previous_state  text,                                       -- restored by an operator unblock
    cursor          jsonb       NOT NULL DEFAULT '{}'::jsonb,
    diagnostic      text,
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE sync_unit (
    unit       text        PRIMARY KEY,                         -- e.g. 'rpo:actual_2026-11-09.json.gz'
    source     text        NOT NULL,
    etag       text,
    sha256     text        NOT NULL,
    records    integer     NOT NULL DEFAULT 0,
    inserted   integer     NOT NULL DEFAULT 0,
    updated    integer     NOT NULL DEFAULT 0,
    absent     integer     NOT NULL DEFAULT 0,
    status     text        NOT NULL CHECK (status IN ('applied', 'rejected')),
    detail     text,
    applied_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE sync_run (
    id          bigserial   PRIMARY KEY,
    source      text        NOT NULL,
    started_at  timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz,
    status      text        NOT NULL DEFAULT 'running' CHECK (status IN ('running', 'ok', 'failed', 'blocked')),
    error       text
);

CREATE TABLE rpo_snapshot_seen (                               -- snapshots fully applied
    snapshot_date   date        PRIMARY KEY,
    parts           integer     NOT NULL,
    records         bigint      NOT NULL,                       -- the next snapshot must keep >= 98 %
    manifest_sha256 text        NOT NULL,
    applied_at      timestamptz NOT NULL DEFAULT now()
);

-- Ids seen in the snapshot being reconciled. Logged on purpose: a reconcile resumes after a crash
-- and the absence marking at its end needs every part's ids.
CREATE TABLE rpo_seen (
    id bigint PRIMARY KEY
);

-- +goose Down

DROP TABLE rpo_seen;
DROP TABLE rpo_snapshot_seen;
DROP TABLE sync_run;
DROP TABLE sync_unit;
DROP TABLE sync_source;
DROP TABLE search_word;
DROP TABLE search_suppression;
DROP TABLE search_token;
DROP TABLE search_name;
DROP TABLE fs_stage;
DROP TABLE rpo_record_stage;
DROP TABLE fs_list_entry;
DROP TABLE fs_vat_payer;
DROP TABLE fs_tax_subject;
DROP TABLE rpo_record;
-- pg_trgm is left installed: other objects may use it.
