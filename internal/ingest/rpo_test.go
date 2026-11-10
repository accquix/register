package ingest_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/accquix/register/internal/ingest"
	"github.com/accquix/register/internal/testkit"
)

func unit(name string, rank int16, data []byte) ingest.RPOUnit {
	return ingest.RPOUnit{Name: name, SHA256: name, Rank: rank, RefreshSearch: true,
		Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }}
}

func one(t *testing.T, conn *pgx.Conn, sql string, args ...any) string {
	t.Helper()
	var s string
	if err := conn.QueryRow(context.Background(), sql, args...).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// A snapshot (rank 1) dated before a daily file must never overwrite what the daily file wrote.
func TestOrderGuard(t *testing.T) {
	ctx := context.Background()
	conn := testkit.Conn(t, testkit.Database(t))
	r := testkit.Company(1, "Poradie Test, s.r.o.")

	newer := r
	newer.Municipality = "Novšie"
	if _, err := ingest.ApplyRPO(ctx, conn, unit("d", 0, testkit.File("2026-01-10", []testkit.Rec{newer}))); err != nil {
		t.Fatal(err)
	}
	older := r
	older.Municipality = "Staršie"
	res, err := ingest.ApplyRPO(ctx, conn, unit("s", 1, testkit.File("2026-01-07", []testkit.Rec{older})))
	if err != nil {
		t.Fatal(err)
	}
	if res.Updated != 0 || one(t, conn, `SELECT municipality FROM rpo_record`) != "Novšie" {
		t.Fatalf("an older snapshot overwrote a newer daily file: %+v", res)
	}
	// the same content again writes nothing
	res, err = ingest.ApplyRPO(ctx, conn, unit("d2", 0, testkit.File("2026-01-11", []testkit.Rec{newer})))
	if err != nil || res.Inserted+res.Updated != 0 {
		t.Fatalf("unchanged content was rewritten: %+v %v", res, err)
	}
}

type failingReader struct {
	data []byte
	at   int
	pos  int
}

func (f *failingReader) Read(p []byte) (int, error) {
	if f.pos >= f.at {
		return 0, errors.New("connection reset")
	}
	n := copy(p, f.data[f.pos:min(f.at, len(f.data))])
	f.pos += n
	return n, nil
}
func (f *failingReader) Close() error { return nil }

// A unit that fails half-way leaves nothing behind; the rerun applies it fully.
func TestUnitIsAtomic(t *testing.T) {
	ctx := context.Background()
	conn := testkit.Conn(t, testkit.Database(t))
	var recs []testkit.Rec
	for i := 1; i <= 200; i++ {
		recs = append(recs, testkit.Company(i, "Atomická Firma"))
	}
	data := testkit.File("2026-01-10", recs)
	u := unit("rpo:atomic", 0, data)
	u.Open = func() (io.ReadCloser, error) { return &failingReader{data: data, at: len(data) / 2}, nil }
	if _, err := ingest.ApplyRPO(ctx, conn, u); err == nil {
		t.Fatal("want an error")
	}
	for _, q := range []string{`SELECT count(*)::text FROM rpo_record`, `SELECT count(*)::text FROM search_name`, `SELECT count(*)::text FROM sync_unit`} {
		if got := one(t, conn, q); got != "0" {
			t.Fatalf("%s = %s after a failed unit", q, got)
		}
	}
	if _, err := ingest.ApplyRPO(ctx, conn, unit("rpo:atomic", 0, data)); err != nil {
		t.Fatal(err)
	}
	if got := one(t, conn, `SELECT count(*)::text FROM rpo_record`); got != "200" {
		t.Fatalf("rerun applied %s records", got)
	}
	if ok, err := ingest.Applied(ctx, conn, "rpo:atomic"); !ok || err != nil {
		t.Fatalf("unit not checkpointed: %v %v", ok, err)
	}
}
