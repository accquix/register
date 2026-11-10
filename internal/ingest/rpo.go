// Package ingest applies source units to the database. Every unit is one transaction:
// TRUNCATE staging, COPY, validate, merge, refresh the search rows it touched, checkpoint.
// A failure at any point rolls the whole unit back, so a rerun starts clean.
package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/accquix/register/internal/sources/rpo"
)

// ErrRejected marks a unit that failed validation. Its data was not applied.
var ErrRejected = errors.New("unit rejected by validation")

// RPOUnit is one RPO file to apply.
type RPOUnit struct {
	Name          string // checkpoint name, e.g. "rpo:actual_2026-11-09.json.gz"
	SHA256        string
	Rank          int16 // 0 daily file, 1 snapshot part (the order guard compares (date, rank))
	Open          func() (io.ReadCloser, error)
	RefreshSearch bool                                                     // update the search layer for touched subjects
	MarkSeen      bool                                                     // record ids in rpo_seen (reconcile)
	CheckDate     func(exportDate time.Time) error                         // nil = accept; an error rejects the unit before anything is staged
	Validate      func(records int) error                                  // nil = accept; an error rejects the unit
	After         func(ctx context.Context, tx pgx.Tx, r UnitResult) error // runs in the same transaction
}

// UnitResult describes an applied unit.
type UnitResult struct {
	Records    int
	Inserted   int
	Updated    int
	ExportDate time.Time
}

var rpoCols = []string{
	"id", "ico", "kind", "name", "name_core", "legal_suffix", "legal_form_code", "legal_form",
	"established_on", "terminated_on", "street", "building_number", "postal_code", "municipality", "country",
	"register_code", "register_name", "registration_office", "registration_number", "registration_valid_from",
	"source_as_of", "src_date", "src_rank", "row_hash",
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullDate(s string) any {
	if s == "" {
		return nil
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return nil
	}
	return t
}

// copySource adapts the streaming decoder to pgx.CopyFromSource.
type copySource struct {
	dec  *rpo.Decoder
	rank int16
	vals []any
}

func (c *copySource) Next() bool {
	if !c.dec.Next() {
		return false
	}
	r := c.dec.Row()
	d := c.dec.ExportDate()
	c.vals = []any{
		r.ID, nullStr(r.ICO), r.Kind, nullStr(r.Name), r.NameCore, nullStr(r.LegalSuffix), nullStr(r.LegalFormCode), nullStr(r.LegalForm),
		nullDate(r.EstablishedOn), nullDate(r.TerminatedOn), nullStr(r.Street), nullStr(r.BuildingNumber), nullStr(r.PostalCode),
		nullStr(r.Municipality), nullStr(r.Country), nullStr(r.RegisterCode), nullStr(r.RegisterName), nullStr(r.RegistrationOffice),
		nullStr(r.RegistrationNumber), nullDate(r.RegistrationValidFrom), d, d, c.rank, r.Hash,
	}
	return true
}
func (c *copySource) Values() ([]any, error) { return c.vals, nil }
func (c *copySource) Err() error             { return c.dec.Err() }

// RowQuerier is satisfied by *pgx.Conn and pgx.Tx.
type RowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Applied reports whether a unit with this name was already applied.
func Applied(ctx context.Context, q RowQuerier, name string) (bool, error) {
	var n int
	err := q.QueryRow(ctx, `SELECT count(*) FROM sync_unit WHERE unit = $1 AND status = 'applied'`, name).Scan(&n)
	return n > 0, err
}

// ApplyRPO applies one RPO file in one transaction.
func ApplyRPO(ctx context.Context, conn *pgx.Conn, u RPOUnit) (UnitResult, error) {
	var res UnitResult
	f, err := u.Open()
	if err != nil {
		return res, err
	}
	defer f.Close()
	dec, err := rpo.NewDecoder(f)
	if err != nil {
		return res, fmt.Errorf("%w: %s: %w", ErrRejected, u.Name, err)
	}
	res.ExportDate = dec.ExportDate()
	if u.CheckDate != nil {
		// The export date becomes src_date, which the order guard compares. A wrong date (above all a
		// future one) would freeze the rows of this file against every later correction.
		if err := u.CheckDate(res.ExportDate); err != nil {
			return res, fmt.Errorf("%w: %s: %w", ErrRejected, u.Name, err)
		}
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		return res, err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `TRUNCATE rpo_record_stage`); err != nil {
		return res, err
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"rpo_record_stage"}, rpoCols, &copySource{dec: dec, rank: u.Rank}); err != nil {
		if derr := dec.Err(); derr != nil {
			return res, fmt.Errorf("%w: %s: %w", ErrRejected, u.Name, derr)
		}
		return res, fmt.Errorf("copy %s: %w", u.Name, err)
	}
	res.Records = dec.Count()
	if u.Validate != nil {
		if err := u.Validate(res.Records); err != nil {
			return res, fmt.Errorf("%w: %s: %w", ErrRejected, u.Name, err)
		}
	}

	if u.MarkSeen {
		if _, err := tx.Exec(ctx, `INSERT INTO rpo_seen (id) SELECT DISTINCT id FROM rpo_record_stage ON CONFLICT DO NOTHING`); err != nil {
			return res, err
		}
	}
	if u.RefreshSearch {
		if err := createTouched(ctx, tx); err != nil {
			return res, err
		}
		if _, err := tx.Exec(ctx, touchedFromStage); err != nil {
			return res, fmt.Errorf("touched: %w", err)
		}
	}
	if err := tx.QueryRow(ctx, mergeRPO).Scan(&res.Inserted, &res.Updated); err != nil {
		return res, fmt.Errorf("merge %s: %w", u.Name, err)
	}
	if u.RefreshSearch {
		if err := refreshTouched(ctx, tx); err != nil {
			return res, err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO sync_unit (unit, source, sha256, records, inserted, updated, status)
VALUES ($1, 'rpo', $2, $3, $4, $5, 'applied')
ON CONFLICT (unit) DO UPDATE SET sha256 = EXCLUDED.sha256, records = EXCLUDED.records, inserted = EXCLUDED.inserted,
  updated = EXCLUDED.updated, status = 'applied', detail = NULL, applied_at = now()`,
		u.Name, u.SHA256, res.Records, res.Inserted, res.Updated); err != nil {
		return res, err
	}
	if u.After != nil {
		if err := u.After(ctx, tx, res); err != nil {
			return res, err
		}
	}
	return res, tx.Commit(ctx)
}

// RecordRejected stores a rejected unit for diagnostics, in its own transaction.
func RecordRejected(ctx context.Context, conn *pgx.Conn, name, source, sha, detail string) error {
	_, err := conn.Exec(ctx, `INSERT INTO sync_unit (unit, source, sha256, status, detail) VALUES ($1, $2, $3, 'rejected', $4)
ON CONFLICT (unit) DO UPDATE SET sha256 = EXCLUDED.sha256, status = 'rejected', detail = EXCLUDED.detail, applied_at = now()`,
		name, source, sha, detail)
	return err
}

// The order guard: a row is replaced only by data of the same or a later (date, rank), and only
// when its exposed content changed (or it had been marked absent). Operator flags such as
// objections live in search_suppression, outside the merged rows.
const changedPredicate = `(r.src_date, r.src_rank) <= (s.src_date, s.src_rank)
  AND (r.row_hash IS DISTINCT FROM s.row_hash OR r.removed_at IS NOT NULL)`

var mergeRPO = func() string {
	set := make([]string, 0, len(rpoCols))
	for _, c := range rpoCols[1:] {
		set = append(set, c+" = EXCLUDED."+c)
	}
	cols := strings.Join(rpoCols, ", ")
	return `WITH s AS (SELECT DISTINCT ON (id) ` + cols + ` FROM rpo_record_stage ORDER BY id),
m AS (
  INSERT INTO rpo_record AS r (` + cols + `) SELECT ` + cols + ` FROM s
  ON CONFLICT (id) DO UPDATE SET ` + strings.Join(set, ", ") + `, changed_at = now(), removed_at = NULL
  WHERE (r.src_date, r.src_rank) <= (EXCLUDED.src_date, EXCLUDED.src_rank)
    AND (r.row_hash IS DISTINCT FROM EXCLUDED.row_hash OR r.removed_at IS NOT NULL)
  RETURNING (xmax = 0) AS inserted)
SELECT count(*) FILTER (WHERE inserted)::int, count(*) FILTER (WHERE NOT inserted)::int FROM m`
}()

// touchedFromStage collects the subject keys the merge is about to change: the new key from the
// staged row and the old key from the stored row (an IČO can appear, change or disappear).
const touchedFromStage = `WITH s AS (SELECT DISTINCT ON (id) id, ico, src_date, src_rank, row_hash FROM rpo_record_stage ORDER BY id)
INSERT INTO touched (skey)
SELECT k FROM (
  SELECT CASE WHEN s.ico IS NOT NULL THEN s.ico ELSE 'r' || s.id END AS k
  FROM s LEFT JOIN rpo_record r ON r.id = s.id
  WHERE r.id IS NULL OR (` + changedPredicate + `)
  UNION
  SELECT CASE WHEN r.ico IS NOT NULL THEN r.ico ELSE 'r' || r.id END
  FROM s JOIN rpo_record r ON r.id = s.id
  WHERE ` + changedPredicate + `
) x
ON CONFLICT DO NOTHING`
