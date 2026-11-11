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
// not affected (D26). See migrations 00003 and 00005 for the model:
//
//   - an objection is the person's request: a number, the key the operator named, when, and when
//     it was revoked;
//   - a link says that the objection covers one RPO record, with why and since when;
//   - a release is the operator's exclusion of one link; it stays, so the sync does not link the
//     record again.

// Link bases.
const (
	BasisAtObjection = "at_objection" // the record carried the IČO when the objection was recorded
	BasisCarriedICO  = "carried_ico"  // the sync saw the record carry the IČO later
	BasisRestored    = "restored"     // put back from a backup
	BasisMigrated    = "migrated"     // carried over by a schema migration
)

// Link is one link between an objection and an RPO record.
type Link struct {
	RecordID       int64
	Basis          string
	Source         string // the sync unit that showed the record with the IČO; "" otherwise
	CreatedAt      time.Time
	ReleasedAt     *time.Time // set when the operator excluded the record from the objection
	ReleasedReason string
}

// Objection is one objection, as recorded and as exported.
type Objection struct {
	ID        int64
	Key       string // what the operator named: an IČO, or "r<RPO id>" for one record
	Reason    string // operator note, never personal data
	CreatedAt time.Time
	RevokedAt *time.Time // set by an undo; the row stays as its record
	Links     []Link
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

// covered selects the records (alias r) that objection $1 has anything to do with: named, linked
// (released or not) or carrying its IČO. Their subject keys are the search rows it can affect.
const covered = `FROM rpo_record r, objection o
WHERE o.id = $1 AND (r.ico = o.ico OR r.id = o.rpo_id OR r.id IN (SELECT rpo_id FROM objection_record WHERE objection_id = o.id))`

// hidden is the complement of notSuppressed for alias r.
const hidden = `NOT (` + notSuppressed + `)`

func refreshObjection(ctx context.Context, tx pgx.Tx, id int64) error {
	rows, err := tx.Query(ctx, `SELECT DISTINCT `+skeyExpr+` `+covered+`
UNION SELECT coalesce(ico, 'r' || rpo_id) FROM objection WHERE id = $1`, id)
	if err != nil {
		return err
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	return RefreshSubjects(ctx, tx, keys)
}

// Coverage says what an objection does right now.
type Coverage struct {
	Records int // records it has to do with
	Hidden  int // of those, how many are out of name search now (by this or any other objection)
}

func coverage(ctx context.Context, tx pgx.Tx, id int64) (Coverage, error) {
	var c Coverage
	err := tx.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE `+hidden+`) `+covered, id).Scan(&c.Records, &c.Hidden)
	return c, err
}

// AddObjection records an objection, links it to the records that carry its IČO now, and removes
// them from the search layer, in one transaction. It waits for a sync unit that is being applied.
// Records 0 means none matches yet: the objection applies when one appears.
func AddObjection(ctx context.Context, conn *pgx.Conn, key, reason string) (id int64, c Coverage, err error) {
	ico, rpoID, err := splitKey(key)
	if err != nil {
		return 0, c, err
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return 0, c, err
	}
	defer tx.Rollback(ctx)
	if err := lockObjections(ctx, tx); err != nil {
		return 0, c, err
	}
	if err := tx.QueryRow(ctx, `INSERT INTO objection (ico, rpo_id, reason) VALUES ($1, $2, nullif($3, '')) RETURNING id`,
		ico, rpoID, reason).Scan(&id); err != nil {
		return 0, c, err
	}
	if _, err := syncObjections(ctx, tx, BasisAtObjection, ""); err != nil {
		return 0, c, err
	}
	if err := refreshObjection(ctx, tx, id); err != nil {
		return 0, c, err
	}
	if c, err = coverage(ctx, tx, id); err != nil {
		return 0, c, err
	}
	return id, c, tx.Commit(ctx)
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
// never touches another objection. The Coverage tells what is true afterwards: of the records the
// objection had to do with, how many are still out of name search because another objection
// covers them.
func RevokeObjection(ctx context.Context, conn *pgx.Conn, id int64) (Coverage, error) {
	var c Coverage
	tx, err := conn.Begin(ctx)
	if err != nil {
		return c, err
	}
	defer tx.Rollback(ctx)
	if err := lockObjections(ctx, tx); err != nil {
		return c, err
	}
	tag, err := tx.Exec(ctx, `UPDATE objection SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, id)
	if err != nil {
		return c, err
	}
	if tag.RowsAffected() == 0 {
		return c, fmt.Errorf("no active objection #%d", id)
	}
	if err := refreshObjection(ctx, tx, id); err != nil {
		return c, err
	}
	if c, err = coverage(ctx, tx, id); err != nil {
		return c, err
	}
	return c, tx.Commit(ctx)
}

// ReleaseLink excludes one record from one objection (release true), or withdraws that exclusion
// (release false). The objection itself and its other links are untouched. A released link stays
// as a row, so the sync does not link the record again. hiddenNow reports whether the record is
// out of name search afterwards (another objection may still cover it).
//
// The record an objection names directly (r<RPO id>) cannot be released: revoke that objection.
func ReleaseLink(ctx context.Context, conn *pgx.Conn, id, recordID int64, reason string, release bool) (hiddenNow bool, err error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if err := lockObjections(ctx, tx); err != nil {
		return false, err
	}
	var active, direct bool
	err = tx.QueryRow(ctx, `SELECT revoked_at IS NULL, coalesce(rpo_id = $2, false) FROM objection WHERE id = $1`, id, recordID).Scan(&active, &direct)
	switch {
	case errors.Is(err, pgx.ErrNoRows) || (err == nil && !active):
		return false, fmt.Errorf("no active objection #%d", id)
	case err != nil:
		return false, err
	case direct:
		return false, fmt.Errorf("objection #%d names record %d itself: revoke the objection instead", id, recordID)
	}
	if release {
		// Also when no link exists yet: the exclusion must be there before a sync sees the record.
		_, err = tx.Exec(ctx, `INSERT INTO objection_record (objection_id, rpo_id, basis, released_at, released_reason)
VALUES ($1, $2, 'at_objection', now(), nullif($3, ''))
ON CONFLICT (objection_id, rpo_id) DO UPDATE SET released_at = now(), released_reason = EXCLUDED.released_reason
WHERE objection_record.released_at IS NULL`, id, recordID, reason)
	} else {
		var tag pgTag
		tag, err = tx.Exec(ctx, `UPDATE objection_record SET released_at = NULL, released_reason = NULL
WHERE objection_id = $1 AND rpo_id = $2 AND released_at IS NOT NULL`, id, recordID)
		if err == nil && tag.RowsAffected() == 0 {
			return false, fmt.Errorf("record %d is not released from objection #%d", recordID, id)
		}
	}
	if err != nil {
		return false, err
	}
	rows, err := tx.Query(ctx, `SELECT `+skeyExpr+` FROM rpo_record r WHERE r.id = $1`, recordID)
	if err != nil {
		return false, err
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return false, err
	}
	if err := RefreshSubjects(ctx, tx, keys); err != nil {
		return false, err
	}
	if err := tx.QueryRow(ctx, `SELECT coalesce((SELECT `+hidden+` FROM rpo_record r WHERE r.id = $1), false)`, recordID).Scan(&hiddenNow); err != nil {
		return false, err
	}
	return hiddenNow, tx.Commit(ctx)
}

type pgTag interface{ RowsAffected() int64 }

// ListObjections returns every objection with its links, revoked ones included, in id order.
func ListObjections(ctx context.Context, conn *pgx.Conn) ([]Objection, error) {
	rows, err := conn.Query(ctx, `SELECT id, coalesce(ico, 'r' || rpo_id), coalesce(reason, ''), created_at, revoked_at FROM objection ORDER BY id`)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Objection, error) {
		var o Objection
		err := r.Scan(&o.ID, &o.Key, &o.Reason, &o.CreatedAt, &o.RevokedAt)
		return o, err
	})
	if err != nil {
		return nil, err
	}
	byID := map[int64]*Objection{}
	for i := range list {
		byID[list[i].ID] = &list[i]
	}
	rows, err = conn.Query(ctx, `SELECT objection_id, rpo_id, basis, coalesce(source, ''), created_at, released_at, coalesce(released_reason, '')
FROM objection_record ORDER BY objection_id, rpo_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var l Link
		if err := rows.Scan(&id, &l.RecordID, &l.Basis, &l.Source, &l.CreatedAt, &l.ReleasedAt, &l.ReleasedReason); err != nil {
			return nil, err
		}
		if o := byID[id]; o != nil {
			o.Links = append(o.Links, l)
		}
	}
	return list, rows.Err()
}

// RestoreObjections puts exported objections back, with their links and releases, and confirms
// the installation's objections as restored, in one transaction. It only adds: an objection
// already present (same key and time) is kept as it is, and none is ever removed, revoked or
// un-revoked by a restore. It returns how many objections were added.
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
		for _, l := range o.Links {
			// An existing link wins, except that a release in the backup is kept: restoring must
			// not bring back a link the operator had excluded.
			if _, err := tx.Exec(ctx, `INSERT INTO objection_record (objection_id, rpo_id, basis, source, created_at, released_at, released_reason)
VALUES ($1, $2, $3, nullif($4, ''), $5, $6, nullif($7, ''))
ON CONFLICT (objection_id, rpo_id) DO UPDATE SET released_at = EXCLUDED.released_at, released_reason = EXCLUDED.released_reason
WHERE objection_record.released_at IS NULL AND EXCLUDED.released_at IS NOT NULL`,
				id, l.RecordID, l.Basis, l.Source, l.CreatedAt, l.ReleasedAt, l.ReleasedReason); err != nil {
				return 0, fmt.Errorf("restore: %w", err)
			}
		}
		ids = append(ids, id)
	}
	// and the records that carry an objected IČO in this database now
	if _, err := syncObjections(ctx, tx, BasisRestored, ""); err != nil {
		return 0, err
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
