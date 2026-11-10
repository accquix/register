package ingest

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// The search layer (D17, D18, D21): search_name holds one row per distinct official name of each
// active, non-suppressed subject; search_token every distinct word (2+ characters) over those
// names; search_word the word dictionary with subject counts.
//
// Subject key: the valid IČO, else 'r' || RPO id. The display name (is_primary) comes from a
// Commercial (code 1) or Trade (code 2) Register record first, else from the latest record.

const skeyExpr = `CASE WHEN r.ico IS NOT NULL THEN r.ico ELSE 'r' || r.id END`

// notSuppressed keeps a record out of the search layer while an active objection covers it: one
// that names the IČO the record carries now, names the record itself, or is anchored to it
// (migration 00003). It is evaluated per record against the current data; nothing is copied to
// other keys, so a wrong IČO in a source file never hides the subject that really owns that IČO.
const notSuppressed = `NOT EXISTS (SELECT 1 FROM objection o WHERE o.revoked_at IS NULL AND o.ico = r.ico)
    AND NOT EXISTS (SELECT 1 FROM objection o WHERE o.revoked_at IS NULL AND o.rpo_id = r.id)
    AND NOT EXISTS (SELECT 1 FROM objection_record a JOIN objection o ON o.id = a.objection_id
                    WHERE a.rpo_id = r.id AND o.revoked_at IS NULL)`

// objectionLock serialises everything that reads the objections to build search rows with
// everything that changes them. Without it, an objection recorded while a unit is being applied
// can miss the rows that unit is about to commit (and the reverse). It is an advisory lock held to
// the end of the transaction, so it needs no table privilege and does not block API reads.
const objectionLock int64 = 0x4163_5265_674f_626a // "AcRegObj"

func lockObjections(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, objectionLock); err != nil {
		return fmt.Errorf("objection lock: %w", err)
	}
	return nil
}

// anchorNewRecords anchors each active objection by IČO to the records that were created with
// that IČO (first seen and never changed since, so they cannot have got it by a later change).
// An existing record that changes to the IČO is hidden while it carries it, but is not anchored:
// if that change was a mistake in the source, its correction ends the effect.
const anchorNewRecords = `INSERT INTO objection_record (objection_id, rpo_id)
SELECT o.id, r.id FROM objection o JOIN rpo_record r ON r.ico = o.ico
WHERE o.revoked_at IS NULL AND r.first_seen_at = r.changed_at AND r.first_seen_at >= o.created_at
ON CONFLICT DO NOTHING`

// syncObjections takes the objection lock and anchors new records. Every writer of the search
// tables calls it before it reads the objections.
func syncObjections(ctx context.Context, tx pgx.Tx) error {
	if err := lockObjections(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, anchorNewRecords); err != nil {
		return fmt.Errorf("objection anchors: %w", err)
	}
	return nil
}

// searchNameSelect builds the rows for search_name. filter limits the records read; the outer
// filter keeps only keys in scope (needed when a record's key changed).
func searchNameSelect(recordFilter, keyFilter string) string {
	return `WITH recs AS (
  SELECT ` + skeyExpr + ` AS skey, r.name_core, r.name, r.ico, r.municipality, r.kind,
         row_number() OVER (PARTITION BY ` + skeyExpr + `
           ORDER BY (coalesce(r.register_code, '') IN ('1', '2')) DESC, r.src_date DESC, r.changed_at DESC, r.id DESC) AS rn
  FROM rpo_record r
  WHERE r.terminated_on IS NULL AND r.removed_at IS NULL
    AND r.name_core <> '' AND r.name IS NOT NULL ` + recordFilter + `
    AND ` + notSuppressed + `
)
SELECT DISTINCT ON (skey, name_core) skey, name_core, name, ico, municipality, kind, rn = 1
FROM recs WHERE true ` + keyFilter + `
ORDER BY skey, name_core, rn`
}

const tokenSelect = `SELECT DISTINCT n.skey, t.token
FROM search_name n, regexp_split_to_table(n.name_core, ' ') AS t(token)
WHERE length(t.token) >= 2`

func createTouched(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `CREATE TEMP TABLE IF NOT EXISTS touched (skey text PRIMARY KEY) ON COMMIT DROP`)
	return err
}

// refreshTouched rebuilds the search rows of the subjects listed in the temp table "touched" and
// recomputes the dictionary counts of every word they had or have.
func refreshTouched(ctx context.Context, tx pgx.Tx) error {
	if err := syncObjections(ctx, tx); err != nil {
		return err
	}
	stmts := []string{
		`CREATE TEMP TABLE IF NOT EXISTS touched_words (word text PRIMARY KEY) ON COMMIT DROP`,
		`INSERT INTO touched_words SELECT DISTINCT token FROM search_token WHERE skey IN (SELECT skey FROM touched) ON CONFLICT DO NOTHING`,
		`DELETE FROM search_token WHERE skey IN (SELECT skey FROM touched)`,
		`DELETE FROM search_name WHERE skey IN (SELECT skey FROM touched)`,
		`INSERT INTO search_name (skey, name_core, name, ico, municipality, kind, is_primary) ` + searchNameSelect(
			`AND (r.ico IN (SELECT skey FROM touched) OR r.id IN (SELECT substr(skey, 2)::bigint FROM touched WHERE skey LIKE 'r%'))`,
			`AND skey IN (SELECT skey FROM touched)`),
		`INSERT INTO search_token (skey, token) ` + tokenSelect + ` AND n.skey IN (SELECT skey FROM touched)`,
		`INSERT INTO touched_words SELECT DISTINCT token FROM search_token WHERE skey IN (SELECT skey FROM touched) ON CONFLICT DO NOTHING`,
		`INSERT INTO search_word (word, freq)
SELECT w.word, (SELECT count(*) FROM search_token t WHERE t.token = w.word) FROM touched_words w
ON CONFLICT (word) DO UPDATE SET freq = EXCLUDED.freq`,
		`DELETE FROM search_word WHERE word IN (SELECT word FROM touched_words) AND freq = 0`,
		`TRUNCATE touched_words`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s); err != nil {
			return fmt.Errorf("search refresh: %w", err)
		}
	}
	return nil
}

// RebuildSearch replaces the whole search layer from rpo_record. Used when a bootstrap publishes.
func RebuildSearch(ctx context.Context, tx pgx.Tx) error {
	if err := syncObjections(ctx, tx); err != nil {
		return err
	}
	stmts := []string{
		`TRUNCATE search_name, search_token, search_word`,
		`INSERT INTO search_name (skey, name_core, name, ico, municipality, kind, is_primary) ` + searchNameSelect("", ""),
		`INSERT INTO search_token (skey, token) ` + tokenSelect,
		`INSERT INTO search_word (word, freq) SELECT token, count(*) FROM search_token GROUP BY token`,
		`ANALYZE search_name`, `ANALYZE search_token`, `ANALYZE search_word`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s); err != nil {
			return fmt.Errorf("search rebuild: %w", err)
		}
	}
	return nil
}

// AbsenceResult reports the absence marking after a snapshot reconcile.
type AbsenceResult struct {
	Active     int64 // rows not marked absent before
	Candidates int64 // rows not in the snapshot and not changed after it
	Marked     bool
}

// MarkAbsent marks rows that the reconciled snapshot no longer contains (rpo_seen holds its ids)
// and that no later daily file changed. If the share exceeds maxRatio, nothing is marked.
func MarkAbsent(ctx context.Context, tx pgx.Tx, snapshot time.Time, maxRatio float64) (AbsenceResult, error) {
	var r AbsenceResult
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM rpo_record WHERE removed_at IS NULL`).Scan(&r.Active); err != nil {
		return r, err
	}
	const cand = `FROM rpo_record r WHERE r.removed_at IS NULL AND r.src_date <= $1
  AND NOT EXISTS (SELECT 1 FROM rpo_seen s WHERE s.id = r.id)`
	if err := tx.QueryRow(ctx, `SELECT count(*) `+cand, snapshot).Scan(&r.Candidates); err != nil {
		return r, err
	}
	if r.Candidates == 0 {
		return r, nil
	}
	if r.Active > 0 && float64(r.Candidates)/float64(r.Active) > maxRatio {
		return r, nil
	}
	if err := createTouched(ctx, tx); err != nil {
		return r, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO touched (skey) SELECT DISTINCT `+skeyExpr+` `+cand+` ON CONFLICT DO NOTHING`, snapshot); err != nil {
		return r, err
	}
	if _, err := tx.Exec(ctx, `UPDATE rpo_record r SET removed_at = now(), changed_at = now() WHERE r.removed_at IS NULL AND r.src_date <= $1
  AND NOT EXISTS (SELECT 1 FROM rpo_seen s WHERE s.id = r.id)`, snapshot); err != nil {
		return r, err
	}
	if err := refreshTouched(ctx, tx); err != nil {
		return r, err
	}
	r.Marked = true
	return r, nil
}

// RefreshSubjects rebuilds the search rows of the given subject keys (used after an operator
// change such as an objection).
func RefreshSubjects(ctx context.Context, tx pgx.Tx, skeys []string) error {
	if err := createTouched(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO touched (skey) SELECT unnest($1::text[]) ON CONFLICT DO NOTHING`, skeys); err != nil {
		return err
	}
	return refreshTouched(ctx, tx)
}
