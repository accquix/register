-- After an upgrade, the search layer is rebuilt before the API serves again.
--
-- A migration can change what is hidden (00003 dropped objections that an earlier rule had copied
-- onto other subjects; 00005 linked more records), but it cannot rebuild the search tables: that
-- is done by the sync. search_rebuild_pending records that the rebuild is owed. The API answers
-- 503 reason=setup while it is set; the next sync pass rebuilds the layer and clears it.
--
-- It also corrects the order date of rows stored by an earlier rule: snapshot rows were ordered by
-- the exportDate inside the file, which could be a day later than the snapshot's name and so
-- outrank the daily file of that day. Updates are ordered by the date in the file name (D35).

-- +goose Up

ALTER TABLE register_setup ADD COLUMN search_rebuild_pending boolean NOT NULL DEFAULT false;

-- Only a database that already published has a search layer to rebuild.
UPDATE register_setup SET search_rebuild_pending = true
WHERE only_row AND EXISTS (SELECT 1 FROM sync_source WHERE source = 'rpo' AND published_as_of IS NOT NULL);

-- No snapshot row can be newer than the snapshot the register is based on.
-- This restores the order for what comes next. It does not bring back a daily change that such a
-- row had already outranked (the daily file of the day after the snapshot, for the records in
-- it): that record stays as the snapshot had it until the next monthly reconcile corrects it.
-- Only databases bootstrapped by a pre-release build are affected; if in doubt, rebuild.
UPDATE rpo_record r SET src_date = s.base_snapshot
FROM sync_source s
WHERE s.source = 'rpo' AND s.base_snapshot IS NOT NULL AND r.src_rank = 1 AND r.src_date > s.base_snapshot;

-- +goose Down

ALTER TABLE register_setup DROP COLUMN search_rebuild_pending;
