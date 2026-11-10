package ingest

import (
	"context"
	"fmt"
	"regexp"
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

// notSuppressed keeps a record out of the search layer when either form of its key is listed in
// search_suppression: the IČO it carries, or its own RPO id. Checking both means an objection
// still holds in the unit where a record gains or loses its IČO.
const notSuppressed = `NOT EXISTS (SELECT 1 FROM search_suppression x WHERE x.skey = r.ico)
    AND NOT EXISTS (SELECT 1 FROM search_suppression x WHERE x.skey = 'r' || r.id)`

// lockSuppression serialises everything that reads search_suppression to build search rows with
// everything that changes it. Without it, an objection recorded while a unit is being applied can
// miss the rows that unit is about to commit (and the reverse). The lock is held to the end of the
// transaction; API reads are not blocked.
const lockSuppression = `LOCK TABLE search_suppression IN SHARE ROW EXCLUSIVE MODE`

// Propagation keeps search_suppression closed over both key forms (see migration 00002):
//  1. a suppressed record ('r' || id) suppresses the IČO that record carries;
//  2. a suppressed IČO suppresses every record that carries it.
//
// It always errs towards hiding. Rows it adds carry the operator's key in origin.
const (
	propagateToICO = `INSERT INTO search_suppression (skey, reason, origin)
SELECT DISTINCT ON (r.ico) r.ico, x.reason, coalesce(x.origin, x.skey)
FROM search_suppression x
JOIN rpo_record r ON r.id = CASE WHEN x.skey ~ '^r[0-9]{1,18}$' THEN substr(x.skey, 2)::bigint END
WHERE r.ico IS NOT NULL
ORDER BY r.ico, x.created_at, x.skey
ON CONFLICT (skey) DO NOTHING`
	propagateToRecords = `INSERT INTO search_suppression (skey, reason, origin)
SELECT DISTINCT ON (r.id) 'r' || r.id, x.reason, coalesce(x.origin, x.skey)
FROM search_suppression x
JOIN rpo_record r ON r.ico = x.skey
ORDER BY r.id, x.created_at, x.skey
ON CONFLICT (skey) DO NOTHING`
)

// syncSuppression takes the suppression lock and propagates objections. With touched set, the
// keys it adds are also put into the temp table "touched", so their search rows are rebuilt.
func syncSuppression(ctx context.Context, tx pgx.Tx, touched bool) error {
	if _, err := tx.Exec(ctx, lockSuppression); err != nil {
		return fmt.Errorf("suppression lock: %w", err)
	}
	for _, stmt := range []string{propagateToICO, propagateToRecords} {
		if touched {
			stmt = `WITH added AS (` + stmt + ` RETURNING skey) INSERT INTO touched (skey) SELECT skey FROM added ON CONFLICT DO NOTHING`
		}
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("suppression propagate: %w", err)
		}
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
	if err := syncSuppression(ctx, tx, true); err != nil {
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
	if err := syncSuppression(ctx, tx, false); err != nil {
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
//
// It waits for a sync unit that is being applied (lockSuppression), so the objection and the search
// rows always agree when it returns. Putting a subject back removes the operator's row and every
// row propagated from it, whichever of them is named.
func Suppress(ctx context.Context, conn *pgx.Conn, skey, reason string, on bool) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, lockSuppression); err != nil {
		return err
	}
	keys := []string{skey}
	if on {
		_, err = tx.Exec(ctx, `INSERT INTO search_suppression (skey, reason) VALUES ($1, nullif($2, ''))
ON CONFLICT (skey) DO UPDATE SET reason = EXCLUDED.reason, origin = NULL`, skey, reason)
	} else {
		var rows pgx.Rows
		rows, err = tx.Query(ctx, `WITH root AS (
  SELECT coalesce((SELECT coalesce(origin, skey) FROM search_suppression WHERE skey = $1), $1) AS k)
DELETE FROM search_suppression s USING root WHERE s.skey = root.k OR s.origin = root.k RETURNING s.skey`, skey)
		if err == nil {
			var removed []string
			if removed, err = pgx.CollectRows(rows, pgx.RowTo[string]); err == nil {
				keys = append(keys, removed...)
			}
		}
	}
	if err != nil {
		return err
	}
	if err := RefreshSubjects(ctx, tx, keys); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// suppressionKey is the only shape a subject key can have: an IČO or 'r' || RPO id.
var suppressionKey = regexp.MustCompile(`^([0-9]{8}|r[0-9]{1,18})$`)

// ValidSuppressionKey reports whether s is an IČO (8 digits) or "r<RPO id>".
func ValidSuppressionKey(s string) bool { return suppressionKey.MatchString(s) }

// Suppression is one row of search_suppression.
type Suppression struct {
	SKey      string
	Origin    string // the operator's key when the row was added by propagation; else ""
	Reason    string
	CreatedAt time.Time
}

// ListSuppressions returns every objection, in key order.
func ListSuppressions(ctx context.Context, conn *pgx.Conn) ([]Suppression, error) {
	rows, err := conn.Query(ctx, `SELECT skey, coalesce(origin, ''), coalesce(reason, ''), created_at FROM search_suppression ORDER BY skey`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Suppression, error) {
		var s Suppression
		err := r.Scan(&s.SKey, &s.Origin, &s.Reason, &s.CreatedAt)
		return s, err
	})
}

// RestoreSuppressions puts exported objections back (after a rebuild of the database) and rebuilds
// the search rows of their subjects, in one transaction. Existing rows are kept; it never removes
// an objection. It returns how many rows were added.
func RestoreSuppressions(ctx context.Context, conn *pgx.Conn, list []Suppression) (int, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, lockSuppression); err != nil {
		return 0, err
	}
	added := 0
	keys := make([]string, 0, len(list))
	for _, s := range list {
		if !ValidSuppressionKey(s.SKey) || (s.Origin != "" && !ValidSuppressionKey(s.Origin)) {
			return 0, fmt.Errorf("restore: %q is not a subject key", s.SKey)
		}
		tag, err := tx.Exec(ctx, `INSERT INTO search_suppression (skey, origin, reason, created_at)
VALUES ($1, nullif($2, ''), nullif($3, ''), $4) ON CONFLICT (skey) DO NOTHING`, s.SKey, s.Origin, s.Reason, s.CreatedAt)
		if err != nil {
			return 0, err
		}
		added += int(tag.RowsAffected())
		keys = append(keys, s.SKey)
	}
	if err := RefreshSubjects(ctx, tx, keys); err != nil {
		return 0, err
	}
	return added, tx.Commit(ctx)
}
