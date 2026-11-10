package ingest

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/accquix/register/internal/sources/fs"
)

// FSUnit is one version of a Financial Administration list.
type FSUnit struct {
	List     fs.List
	Name     string // checkpoint name, e.g. "fs:ds_dphs:<sha prefix>"
	SHA256   string
	Path     string    // local ZIP
	PrevAsOf time.Time // list date of the version applied before; zero if none
	After    func(ctx context.Context, tx pgx.Tx, r FSResult) error
}

// FSResult describes an applied list version.
type FSResult struct {
	AsOf       time.Time
	Items      int   // items in the file
	Distinct   int   // distinct keys
	Duplicates int   // items dropped as duplicates of a key
	Conflicts  int   // keys whose duplicates differ in content (kept: the lowest row hash)
	PrevActive int64 // active rows before
	Inserted   int
	Updated    int
	Absent     int64 // rows marked absent
}

var fsStageCols = []string{"k", "dataset", "ic_dph", "dic", "ico", "name", "name_core", "municipality", "postal_code", "street",
	"country", "paragraph", "d1", "d2", "d3", "attrs", "source_as_of", "row_hash"}

type fsCopy struct {
	dec  *fs.Decoder
	slug string
	vals []any
}

func (c *fsCopy) Next() bool {
	if !c.dec.Next() {
		return false
	}
	r := c.dec.Row()
	var attrs any
	if r.Attrs != "" {
		attrs = r.Attrs
	}
	c.vals = []any{r.Key, c.slug, nullStr(r.ICDPH), nullStr(r.DIC), nullStr(r.ICO), nullStr(r.Name), r.NameCore, nullStr(r.Municipality),
		nullStr(r.PostalCode), nullStr(r.Street), nullStr(r.Country), nullStr(r.Paragraph), nullDate(r.D1), nullDate(r.D2), nullDate(r.D3),
		attrs, c.dec.AsOf(), r.Hash}
	return true
}
func (c *fsCopy) Values() ([]any, error) { return c.vals, nil }
func (c *fsCopy) Err() error             { return c.dec.Err() }

type fsTarget struct {
	table, key, cols, sel, scope string
}

func targetFor(l fs.List) fsTarget {
	switch l.Target {
	case fs.TaxSubjects:
		return fsTarget{"fs_tax_subject", "dic",
			"dic, ico, name, name_core, municipality, postal_code, street, country, source_as_of, row_hash",
			"k, ico, name, name_core, municipality, postal_code, street, country, source_as_of, row_hash", "true"}
	case fs.VATPayers:
		return fsTarget{"fs_vat_payer", "vkey",
			"vkey, ic_dph, ico, name, name_core, paragraph, registered_on, type_changed_on, payer_from, municipality, postal_code, street, source_as_of, row_hash",
			"k, ic_dph, ico, name, name_core, paragraph, d1, d2, d3, municipality, postal_code, street, source_as_of, row_hash", "true"}
	default:
		return fsTarget{"fs_list_entry", "subject_key",
			"dataset, subject_key, ic_dph, dic, ico, name, attrs, source_as_of, row_hash",
			"dataset, k, ic_dph, dic, ico, name, coalesce(attrs, '{}'::jsonb), source_as_of, row_hash", "t.dataset = '" + l.Slug + "'"}
	}
}

// ApplyFS validates and merges one list version in one transaction (D23):
// list date not older than the applied one, 0 parse errors, row-count drop and absent share at
// most List.MaxDrop. Duplicate keys keep the row with the lowest content hash (deterministic).
func ApplyFS(ctx context.Context, conn *pgx.Conn, u FSUnit) (FSResult, error) {
	var res FSResult
	dec, err := fs.Open(u.List.Slug, u.Path)
	if err != nil {
		return res, fmt.Errorf("%w: %s: %w", ErrRejected, u.Name, err)
	}
	defer func() { _ = dec.Close() }()

	tx, err := conn.Begin(ctx)
	if err != nil {
		return res, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `TRUNCATE fs_stage`); err != nil {
		return res, err
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"fs_stage"}, fsStageCols, &fsCopy{dec: dec, slug: u.List.Slug}); err != nil {
		if derr := dec.Err(); derr != nil {
			return res, fmt.Errorf("%w: %s: %w", ErrRejected, u.Name, derr)
		}
		return res, fmt.Errorf("copy %s: %w", u.Name, err)
	}
	if derr := dec.Err(); derr != nil {
		return res, fmt.Errorf("%w: %s: %w", ErrRejected, u.Name, derr)
	}
	res.AsOf, res.Items = dec.AsOf(), dec.Count()
	if !u.PrevAsOf.IsZero() && res.AsOf.Before(u.PrevAsOf) {
		return res, fmt.Errorf("%w: %s: list date %s is older than the applied %s", ErrRejected, u.Name,
			res.AsOf.Format("2006-01-02"), u.PrevAsOf.Format("2006-01-02"))
	}
	if err := tx.QueryRow(ctx, `SELECT count(DISTINCT k)::int,
  (SELECT count(*)::int FROM (SELECT k FROM fs_stage GROUP BY k HAVING count(DISTINCT row_hash) > 1) c) FROM fs_stage`).Scan(&res.Distinct, &res.Conflicts); err != nil {
		return res, err
	}
	res.Duplicates = res.Items - res.Distinct

	t := targetFor(u.List)
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+t.table+` t WHERE t.absent_since IS NULL AND `+t.scope).Scan(&res.PrevActive); err != nil {
		return res, err
	}
	if res.PrevActive > 0 && float64(res.Distinct) < float64(res.PrevActive)*(1-u.List.MaxDrop) {
		return res, fmt.Errorf("%w: %s: %d rows, %d active before (maximum drop %.0f %%)", ErrRejected, u.Name,
			res.Distinct, res.PrevActive, u.List.MaxDrop*100)
	}
	var absent int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+t.table+` t WHERE t.absent_since IS NULL AND `+t.scope+
		` AND NOT EXISTS (SELECT 1 FROM fs_stage s WHERE s.k = t.`+t.key+`)`).Scan(&absent); err != nil {
		return res, err
	}
	if res.PrevActive > 0 && float64(absent) > float64(res.PrevActive)*u.List.MaxDrop {
		return res, fmt.Errorf("%w: %s: %d of %d active rows would be marked absent (maximum %.0f %%)", ErrRejected, u.Name,
			absent, res.PrevActive, u.List.MaxDrop*100)
	}

	conflict := t.key
	if u.List.Target == fs.ListEntries {
		conflict = "dataset, subject_key"
	}
	var set []string
	for _, c := range strings.Split(t.cols, ", ") {
		if c != t.key && c != "dataset" {
			set = append(set, c+" = EXCLUDED."+c)
		}
	}
	merge := `WITH m AS (
  INSERT INTO ` + t.table + ` AS t (` + t.cols + `)
  SELECT DISTINCT ON (k) ` + t.sel + ` FROM fs_stage ORDER BY k, row_hash
  ON CONFLICT (` + conflict + `) DO UPDATE SET ` + strings.Join(set, ", ") + `, changed_at = now(), absent_since = NULL
  WHERE t.row_hash IS DISTINCT FROM EXCLUDED.row_hash OR t.absent_since IS NOT NULL
  RETURNING (xmax = 0) AS inserted)
SELECT count(*) FILTER (WHERE inserted)::int, count(*) FILTER (WHERE NOT inserted)::int FROM m`
	if err := tx.QueryRow(ctx, merge).Scan(&res.Inserted, &res.Updated); err != nil {
		return res, fmt.Errorf("merge %s: %w", u.Name, err)
	}
	tag, err := tx.Exec(ctx, `UPDATE `+t.table+` t SET absent_since = now() WHERE t.absent_since IS NULL AND `+t.scope+
		` AND NOT EXISTS (SELECT 1 FROM fs_stage s WHERE s.k = t.`+t.key+`)`)
	if err != nil {
		return res, err
	}
	res.Absent = tag.RowsAffected()

	detail := fmt.Sprintf("list date %s; %d duplicates (%d conflicting)", res.AsOf.Format("2006-01-02"), res.Duplicates, res.Conflicts)
	if _, err := tx.Exec(ctx, `INSERT INTO sync_unit (unit, source, sha256, records, inserted, updated, absent, status, detail)
VALUES ($1, $2, $3, $4, $5, $6, $7, 'applied', $8)
ON CONFLICT (unit) DO UPDATE SET sha256 = EXCLUDED.sha256, records = EXCLUDED.records, inserted = EXCLUDED.inserted,
  updated = EXCLUDED.updated, absent = EXCLUDED.absent, status = 'applied', detail = EXCLUDED.detail, applied_at = now()`,
		u.Name, "fs:"+u.List.Slug, u.SHA256, res.Items, res.Inserted, res.Updated, res.Absent, detail); err != nil {
		return res, err
	}
	if u.After != nil {
		if err := u.After(ctx, tx, res); err != nil {
			return res, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return res, err
	}
	return res, nil
}
