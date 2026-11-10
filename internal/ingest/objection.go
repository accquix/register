package ingest

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// Objections (GDPR): a person asks to be left out of name search. Exact lookup by identifier is
// not affected (D26). See migration 00003 for the model.

// Objection is one objection, as recorded and as exported.
type Objection struct {
	ID        int64
	Key       string // what the operator named: an IČO, or "r<RPO id>" for one record
	Reason    string // operator note, never personal data
	CreatedAt time.Time
	RevokedAt *time.Time // set by an undo; the row stays as its record
	Anchors   []int64    // RPO records the objection is anchored to
}

var objectionKey = regexp.MustCompile(`^([0-9]{8}|r[1-9][0-9]{0,17})$`)

// ValidObjectionKey reports whether s is an IČO (8 digits) or "r<RPO id>".
func ValidObjectionKey(s string) bool { return objectionKey.MatchString(s) }

// splitKey turns a key into the two nullable columns of the objection table.
func splitKey(key string) (ico *string, rpoID *int64, err error) {
	if !ValidObjectionKey(key) {
		return nil, nil, fmt.Errorf("%q is not an IČO (8 digits) or r<RPO id>", key)
	}
	if key[0] == 'r' {
		n, _ := strconv.ParseInt(key[1:], 10, 64)
		return nil, &n, nil
	}
	return &key, nil, nil
}

// ErrSetupUnconfirmed is returned by commands that need the operator's confirmation first.
var ErrSetupUnconfirmed = errors.New("the objections of this installation are not confirmed: run `suppress --restore FILE` to put a backup back, or `suppress --init-empty` for a new installation with none")

// coveredKeys lists the subject keys whose search rows depend on objection id: the keys of the
// records it covers now, and its own key.
const coveredKeys = `SELECT DISTINCT ` + skeyExpr + ` FROM rpo_record r, objection o
WHERE o.id = $1 AND (r.ico = o.ico OR r.id = o.rpo_id OR r.id IN (SELECT rpo_id FROM objection_record WHERE objection_id = o.id))
UNION SELECT coalesce(ico, 'r' || rpo_id) FROM objection WHERE id = $1`

func refreshObjection(ctx context.Context, tx pgx.Tx, id int64) error {
	rows, err := tx.Query(ctx, coveredKeys, id)
	if err != nil {
		return err
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	return RefreshSubjects(ctx, tx, keys)
}

// AddObjection records an objection and removes the subject from the search layer, in one
// transaction. It waits for a sync unit that is being applied. records is the number of RPO
// records it covers now; 0 means none matches yet (the objection applies when one appears).
func AddObjection(ctx context.Context, conn *pgx.Conn, key, reason string) (id int64, records int, err error) {
	ico, rpoID, err := splitKey(key)
	if err != nil {
		return 0, 0, err
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback(ctx)
	if err := lockObjections(ctx, tx); err != nil {
		return 0, 0, err
	}
	if err := tx.QueryRow(ctx, `INSERT INTO objection (ico, rpo_id, reason) VALUES ($1, $2, nullif($3, '')) RETURNING id`,
		ico, rpoID, reason).Scan(&id); err != nil {
		return 0, 0, err
	}
	// Anchor to the records that carry the IČO today: they stay hidden if they lose it later.
	if _, err := tx.Exec(ctx, `INSERT INTO objection_record (objection_id, rpo_id)
SELECT o.id, r.id FROM objection o JOIN rpo_record r ON r.ico = o.ico WHERE o.id = $1 ON CONFLICT DO NOTHING`, id); err != nil {
		return 0, 0, err
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM rpo_record r, objection o WHERE o.id = $1 AND (r.ico = o.ico OR r.id = o.rpo_id)`, id).Scan(&records); err != nil {
		return 0, 0, err
	}
	if err := refreshObjection(ctx, tx, id); err != nil {
		return 0, 0, err
	}
	return id, records, tx.Commit(ctx)
}

// ActiveObjections returns the ids of the active objections recorded for key.
func ActiveObjections(ctx context.Context, conn *pgx.Conn, key string) ([]int64, error) {
	ico, rpoID, err := splitKey(key)
	if err != nil {
		return nil, err
	}
	rows, err := conn.Query(ctx, `SELECT id FROM objection WHERE revoked_at IS NULL AND (ico = $1 OR rpo_id = $2) ORDER BY id`, ico, rpoID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

// RevokeObjection undoes one objection, by its id, and rebuilds the search rows it affected. It
// never touches another objection. stillHiddenBy lists the other active objections that still
// cover a record this one covered: with any, the subject (or part of it) stays out of name search.
func RevokeObjection(ctx context.Context, conn *pgx.Conn, id int64) (stillHiddenBy []int64, err error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if err := lockObjections(ctx, tx); err != nil {
		return nil, err
	}
	tag, err := tx.Exec(ctx, `UPDATE objection SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, id)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, fmt.Errorf("no active objection #%d", id)
	}
	rows, err := tx.Query(ctx, `WITH mine AS (
  SELECT r.id, r.ico FROM rpo_record r, objection o
  WHERE o.id = $1 AND (r.ico = o.ico OR r.id = o.rpo_id OR r.id IN (SELECT rpo_id FROM objection_record WHERE objection_id = o.id)))
SELECT DISTINCT o.id FROM objection o, mine m
WHERE o.revoked_at IS NULL AND o.id <> $1
  AND (o.ico = m.ico OR o.rpo_id = m.id OR m.id IN (SELECT rpo_id FROM objection_record WHERE objection_id = o.id))
ORDER BY o.id`, id)
	if err != nil {
		return nil, err
	}
	if stillHiddenBy, err = pgx.CollectRows(rows, pgx.RowTo[int64]); err != nil {
		return nil, err
	}
	if err := refreshObjection(ctx, tx, id); err != nil {
		return nil, err
	}
	return stillHiddenBy, tx.Commit(ctx)
}

// ListObjections returns every objection, revoked ones included, in id order.
func ListObjections(ctx context.Context, conn *pgx.Conn) ([]Objection, error) {
	rows, err := conn.Query(ctx, `SELECT o.id, coalesce(o.ico, 'r' || o.rpo_id), coalesce(o.reason, ''), o.created_at, o.revoked_at,
  coalesce((SELECT array_agg(a.rpo_id ORDER BY a.rpo_id) FROM objection_record a WHERE a.objection_id = o.id), '{}')
FROM objection o ORDER BY o.id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Objection, error) {
		var o Objection
		err := r.Scan(&o.ID, &o.Key, &o.Reason, &o.CreatedAt, &o.RevokedAt, &o.Anchors)
		return o, err
	})
}

// RestoreObjections puts exported objections back and confirms the installation's objections as
// restored, in one transaction. It only adds: an objection already present (same key and time) is
// skipped, and none is ever removed or revoked by a restore. It returns how many were added.
func RestoreObjections(ctx context.Context, conn *pgx.Conn, list []Objection) (int, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if err := lockObjections(ctx, tx); err != nil {
		return 0, err
	}
	added := 0
	var ids []int64
	for _, o := range list {
		ico, rpoID, err := splitKey(o.Key)
		if err != nil {
			return 0, fmt.Errorf("restore: %w", err)
		}
		var id int64
		err = tx.QueryRow(ctx, `SELECT id FROM objection WHERE created_at = $3 AND (ico = $1 OR rpo_id = $2)`, ico, rpoID, o.CreatedAt).Scan(&id)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			if err := tx.QueryRow(ctx, `INSERT INTO objection (ico, rpo_id, reason, created_at, revoked_at)
VALUES ($1, $2, nullif($3, ''), $4, $5) RETURNING id`, ico, rpoID, o.Reason, o.CreatedAt, o.RevokedAt).Scan(&id); err != nil {
				return 0, err
			}
			added++
		case err != nil:
			return 0, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO objection_record (objection_id, rpo_id) SELECT $1, unnest($2::bigint[]) ON CONFLICT DO NOTHING`, id, o.Anchors); err != nil {
			return 0, err
		}
		// and the records that carry the IČO in this database now
		if _, err := tx.Exec(ctx, `INSERT INTO objection_record (objection_id, rpo_id)
SELECT o.id, r.id FROM objection o JOIN rpo_record r ON r.ico = o.ico WHERE o.id = $1 AND o.revoked_at IS NULL ON CONFLICT DO NOTHING`, id); err != nil {
			return 0, err
		}
		ids = append(ids, id)
	}
	for _, id := range ids {
		if err := refreshObjection(ctx, tx, id); err != nil {
			return 0, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE register_setup SET objections_confirmed_at = now(), objections_confirmed_as = 'restored' WHERE only_row`); err != nil {
		return 0, err
	}
	return added, tx.Commit(ctx)
}

// ConfirmNoObjections records that this is a new installation with no objections to restore.
// It refuses when objections exist or the installation is already confirmed.
func ConfirmNoObjections(ctx context.Context, conn *pgx.Conn) error {
	tag, err := conn.Exec(ctx, `UPDATE register_setup SET objections_confirmed_at = now(), objections_confirmed_as = 'new'
WHERE only_row AND objections_confirmed_at IS NULL AND NOT EXISTS (SELECT 1 FROM objection)`)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errors.New("not confirmed: the installation is already confirmed, or objections exist")
	}
	return nil
}

// SetupConfirmed reports whether the operator confirmed the objections of this installation.
func SetupConfirmed(ctx context.Context, q RowQuerier) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `SELECT coalesce((SELECT objections_confirmed_at IS NOT NULL FROM register_setup WHERE only_row), false)`).Scan(&ok)
	return ok, err
}
