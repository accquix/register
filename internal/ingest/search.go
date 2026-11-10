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
    AND NOT EXISTS (SELECT 1 FROM search_suppression x WHERE x.skey = ` + skeyExpr + `)
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
// change such as a suppression).
func RefreshSubjects(ctx context.Context, tx pgx.Tx, skeys []string) error {
	if err := createTouched(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO touched (skey) SELECT unnest($1::text[]) ON CONFLICT DO NOTHING`, skeys); err != nil {
		return err
	}
	return refreshTouched(ctx, tx)
}

// Suppress keeps a subject (IČO, or "r<RPO id>") out of name search, or puts it back (D26 and
// architecture "Objections"). Exact lookup by identifier is unaffected.
func Suppress(ctx context.Context, conn *pgx.Conn, skey, reason string, on bool) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if on {
		_, err = tx.Exec(ctx, `INSERT INTO search_suppression (skey, reason) VALUES ($1, nullif($2, ''))
ON CONFLICT (skey) DO UPDATE SET reason = EXCLUDED.reason`, skey, reason)
	} else {
		_, err = tx.Exec(ctx, `DELETE FROM search_suppression WHERE skey = $1`, skey)
	}
	if err != nil {
		return err
	}
	if err := RefreshSubjects(ctx, tx, []string{skey}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
