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
	"search_name", "search_token", "search_word", "sync_source", "sync_unit", "sync_run", "rpo_snapshot_seen",
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
