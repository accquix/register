package migrate

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

// testDatabase creates a fresh database on the server named by REGISTER_TEST_DATABASE_URL and
// returns its DSN. The test is skipped when the variable is unset.
func testDatabase(t *testing.T) string {
	t.Helper()
	admin := os.Getenv("REGISTER_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("REGISTER_TEST_DATABASE_URL not set; skipping database test")
	}
	ctx := context.Background()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	name := "register_test_" + hex.EncodeToString(b)
	c, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := c.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	c.Close(ctx)
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), admin)
		if err == nil {
			c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			c.Close(context.Background())
		}
	})
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

var expectedTables = []string{
	"rpo_record", "fs_tax_subject", "fs_vat_payer", "fs_list_entry", "rpo_record_stage", "fs_stage",
	"search_name", "search_token", "search_word", "objection", "objection_record", "register_setup", "sync_source", "sync_unit", "sync_run", "rpo_snapshot_seen", "rpo_seen",
}

func tableCount(t *testing.T, dsn string) int {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	var n int
	if err := c.QueryRow(ctx, `SELECT count(*) FROM pg_tables WHERE schemaname = 'public' AND tablename = ANY($1)`, expectedTables).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestUpDownUp(t *testing.T) {
	dsn := testDatabase(t)
	ctx := context.Background()

	n, err := Up(ctx, dsn)
	if err != nil {
		t.Fatalf("up: %v", err)
	}
	if n < 1 {
		t.Fatalf("up applied %d migrations", n)
	}
	if got := tableCount(t, dsn); got != len(expectedTables) {
		t.Fatalf("after up: %d of %d tables", got, len(expectedTables))
	}
	if n, err := Up(ctx, dsn); err != nil || n != 0 {
		t.Fatalf("second up should be a no-op, got (%d, %v)", n, err)
	}

	st, err := Statuses(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range st {
		if !s.Applied {
			t.Errorf("migration %d (%s) not applied", s.Version, s.Source)
		}
	}

	if _, err := DownTo(ctx, dsn, 0); err != nil {
		t.Fatalf("down: %v", err)
	}
	if got := tableCount(t, dsn); got != 0 {
		t.Fatalf("after down: %d tables left", got)
	}
	if _, err := Up(ctx, dsn); err != nil {
		t.Fatalf("up again: %v", err)
	}
}

// TestSchemaConstraints checks a few invariants the API relies on.
func TestSchemaConstraints(t *testing.T) {
	dsn := testDatabase(t)
	ctx := context.Background()
	if _, err := Up(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)

	// An IČ DPH may appear for several subjects (VAT groups): two rows, same ic_dph.
	for _, k := range []string{"SK2099999999|12345679", "SK2099999999|98765434"} {
		if _, err := c.Exec(ctx, `INSERT INTO fs_vat_payer (vkey, ic_dph, paragraph, source_as_of, row_hash) VALUES ($1, 'SK2099999999', '§4b', '2026-11-10', '\x00')`, k); err != nil {
			t.Fatalf("VAT group member %s: %v", k, err)
		}
	}
	// An IČO that is not 8 digits is rejected (the normaliser maps it to NULL).
	if _, err := c.Exec(ctx, `INSERT INTO rpo_record (id, ico, kind, source_as_of, src_date, src_rank, row_hash) VALUES (1, 'Neuvedené', 'other', '2026-11-07', '2026-11-07', 1, '\x00')`); err == nil {
		t.Fatal("a non-numeric IČO was accepted")
	}
	// The trigram index exists only on the word dictionary (D29).
	var n int
	if err := c.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE schemaname = 'public' AND indexdef ILIKE '%gin_trgm_ops%'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("want exactly 1 trigram index, got %d", n)
	}
}

// Migration 00003 carries existing objections into the new model at once: the operator's rows
// become objections, record keys propagated by 00002 become anchors, keys propagated onto another
// IČO are dropped, and a database that already published stays confirmed.
func TestObjectionsAreCarriedOver(t *testing.T) {
	dsn := testDatabase(t)
	ctx := context.Background()
	p, db, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.UpTo(ctx, 2); err != nil {
		t.Fatal(err)
	}
	db.Close()
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	if _, err := c.Exec(ctx, `
INSERT INTO rpo_record (id, ico, kind, source_as_of, src_date, src_rank, row_hash) VALUES
  (900000004, '99900004', 'sole_trader', '2026-11-07', '2026-11-07', 1, '\x00'),
  (900000005, NULL,       'sole_trader', '2026-11-07', '2026-11-07', 1, '\x00'),
  (900000001, '99900001', 'legal_person', '2026-11-07', '2026-11-07', 1, '\x00');
INSERT INTO search_suppression (skey, reason, origin) VALUES
  ('99900004', 'ticket 1', NULL),          -- the operator's objection
  ('r900000005', 'ticket 1', '99900004'),  -- a record that once carried the IČO
  ('99900001', 'ticket 1', '99900004'),    -- spread onto another subject by a wrong IČO (the defect)
  ('r900000009', 'ticket 2', NULL);        -- an objection for one record
INSERT INTO sync_source (source, state, published_as_of) VALUES ('rpo', 'daily', '2026-11-07')`); err != nil {
		t.Fatal(err)
	}
	if _, err := Up(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	rows, err := c.Query(ctx, `SELECT coalesce(o.ico, 'r' || o.rpo_id) || ' ' || o.reason || ' ' ||
  coalesce((SELECT string_agg(a.rpo_id::text, ',' ORDER BY a.rpo_id) FROM objection_record a WHERE a.objection_id = o.id), '-')
FROM objection o ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"99900004 ticket 1 900000004,900000005", "r900000009 ticket 2 -"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("objections after the migration:\n got  %v\n want %v", got, want)
	}
	var as string
	if err := c.QueryRow(ctx, `SELECT objections_confirmed_as FROM register_setup`).Scan(&as); err != nil || as != "existing" {
		t.Fatalf("a published database must stay confirmed: %q %v", as, err)
	}
}

// Migration 00005 on an existing database: every record that carries an objected IČO is linked at
// once (the earlier rule had linked only some), existing links keep working, a revoked objection
// gets no new links, and the API's view leaves an objection with a released record out of the
// by-IČO filter.
func TestLinksAreCompletedOnAnExistingDatabase(t *testing.T) {
	dsn := testDatabase(t)
	ctx := context.Background()
	p, db, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.UpTo(ctx, 4); err != nil {
		t.Fatal(err)
	}
	db.Close()
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	if _, err := c.Exec(ctx, `
INSERT INTO rpo_record (id, ico, kind, source_as_of, src_date, src_rank, row_hash) VALUES
  (900000004, '99900004', 'sole_trader', '2026-11-07', '2026-11-07', 1, '\x00'),
  (900000005, '99900004', 'sole_trader', '2026-11-07', '2026-11-07', 1, '\x00'),  -- carries the IČO, never linked
  (900000006, NULL,       'sole_trader', '2026-11-07', '2026-11-07', 1, '\x00'),  -- linked earlier, lost the IČO
  (900000007, '99900007', 'sole_trader', '2026-11-07', '2026-11-07', 1, '\x00');
INSERT INTO objection (ico, reason) VALUES ('99900004', 'ticket 1');
INSERT INTO objection (ico, reason, revoked_at) VALUES ('99900007', 'ticket 2', now());
INSERT INTO objection_record (objection_id, rpo_id) VALUES (1, 900000004), (1, 900000006)`); err != nil {
		t.Fatal(err)
	}
	if _, err := Up(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	rows, err := c.Query(ctx, `SELECT objection_id || ' ' || rpo_id || ' ' || basis FROM objection_record ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"1 900000004 migrated", "1 900000005 migrated", "1 900000006 migrated"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("links after the migration:\n got  %v\n want %v", got, want)
	}
	var n int
	if err := c.QueryRow(ctx, `SELECT count(*) FROM objection_active WHERE ico = '99900004'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the view must list the active objection by IČO: %d %v", n, err)
	}
	if _, err := c.Exec(ctx, `UPDATE objection_record SET released_at = now() WHERE rpo_id = 900000005`); err != nil {
		t.Fatal(err)
	}
	if err := c.QueryRow(ctx, `SELECT count(*) FROM objection_active WHERE ico = '99900004'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("an objection with a released record must leave the by-IČO filter: %d %v", n, err)
	}
	// down and up again keeps the objections and the links
	if _, err := DownTo(ctx, dsn, 4); err != nil {
		t.Fatal(err)
	}
	if _, err := Up(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	if err := c.QueryRow(ctx, `SELECT count(*) FROM objection_record`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("links after down and up: %d %v", n, err)
	}
}
