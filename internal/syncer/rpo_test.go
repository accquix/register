package syncer_test

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/accquix/register/internal/sources/rpo"
	"github.com/accquix/register/internal/syncer"
	"github.com/accquix/register/internal/testkit"
)

func newSync(t *testing.T, conn *pgx.Conn, b *testkit.Bucket) *syncer.RPO {
	bk := rpo.NewBucket(b.URL())
	bk.Retries = 1
	return syncer.NewRPO(conn, syncer.RPOConfig{
		Bucket: bk, WorkDir: t.TempDir(), Concurrency: 2,
		PartRecords: 3, DailyMaxRecords: 4, SnapshotMinRatio: 0.5, AbsenceMaxRatio: 0.5,
	})
}

func state(t *testing.T, conn *pgx.Conn) syncer.State {
	t.Helper()
	st, err := syncer.LoadState(context.Background(), conn, "rpo")
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func strs(t *testing.T, conn *pgx.Conn, sql string, args ...any) []string {
	t.Helper()
	rows, err := conn.Query(context.Background(), sql, args...)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func num(t *testing.T, conn *pgx.Conn, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func eq(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("%s:\n got  %v\n want %v", what, got, want)
	}
}

func TestRPOLifecycle(t *testing.T) {
	ctx := context.Background()
	dsn := testkit.Database(t)
	conn := testkit.Conn(t, dsn)
	b := testkit.NewBucket(t)
	s := newSync(t, conn, b)

	alfa := testkit.Company(1, "Alfa Stavby, s.r.o.")
	beta := testkit.Company(2, "Beta Pekáreň, s.r.o.")
	gama := testkit.Company(3, "Gama Servis, a.s.")
	gama.LegalCode = "121"
	trader := testkit.Trader(4, "99900004", "Ján Fiktívny - Kaderníctvo") // same IČO, two registrations:
	agent := testkit.Trader(5, "99900004", "Ján Fiktívny")                // second record of the same person
	agent.RegisterCode, agent.RegisterName = "9", "Register finančných agentov"
	noico := testkit.Trader(6, "Neuvedené", "Bez IČO Remeslo")

	// --- 1. nothing complete yet: a manifest without all parts waits ---
	b.Put("batch-init/init_2026-01-03_list.txt", []byte("init_2026-01-03_001.json.gz\n"))
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if st := state(t, conn); st.State != "empty" {
		t.Fatalf("incomplete snapshot: state %q", st.State)
	}

	// --- 2. bootstrap: snapshot (parts of 3) + two daily files ---
	b.PutSnapshot("2026-01-03", []testkit.Rec{alfa, beta, gama, trader, agent, noico}, 3)
	beta2 := beta
	beta2.Municipality = "Nové Fiktívne"
	b.PutDaily("2026-01-04", []testkit.Rec{beta2})
	b.PutDaily("2026-01-05", nil)
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	st := state(t, conn)
	if st.State != "daily" || st.BaseSnapshot != "2026-01-03" || st.AppliedThrough != "2026-01-05" || st.PublishedAsOf != "2026-01-05" {
		t.Fatalf("after bootstrap: %+v", st)
	}
	if n := num(t, conn, `SELECT count(*) FROM rpo_record`); n != 6 {
		t.Fatalf("records: %d", n)
	}
	if m := strs(t, conn, `SELECT municipality FROM rpo_record WHERE id = $1`, beta.ID); m[0] != "Nové Fiktívne" {
		t.Fatalf("daily after the snapshot not applied: %v", m)
	}
	// subjects: one per IČO, the IČO-less record by its RPO id; both names of the two-record trader are searchable
	eq(t, "search keys", strs(t, conn, `SELECT DISTINCT skey FROM search_name`),
		[]string{"99900001", "99900002", "99900003", "99900004", "r900000006"})
	eq(t, "trader names", strs(t, conn, `SELECT name_core FROM search_name WHERE skey = '99900004'`),
		[]string{"jan fiktivny", "jan fiktivny kadernictvo"})
	eq(t, "trader primary", strs(t, conn, `SELECT name FROM search_name WHERE skey = '99900004' AND is_primary`),
		[]string{"Ján Fiktívny - Kaderníctvo"}) // Trade Register record wins over the agents register
	if n := num(t, conn, `SELECT freq FROM search_word WHERE word = 'fiktivny'`); n != 1 {
		t.Fatalf("word frequency counts subjects, got %d", n)
	}
	if n := num(t, conn, `SELECT count(*) FROM rpo_record WHERE name ILIKE '%Štatutár%' OR street = 'Bydlisková'`); n != 0 {
		t.Fatal("personal data of statutory bodies was stored")
	}

	// --- 3. a second run is a no-op ---
	gets := b.Gets["batch-daily/actual_2026-01-05.json.gz"]
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if b.Gets["batch-daily/actual_2026-01-05.json.gz"] != gets {
		t.Fatal("an applied file was downloaded again")
	}

	// --- 4. daily: rename one subject, terminate another ---
	alfa2 := alfa
	alfa2.Name = "Alfa Domy, s.r.o."
	gama2 := gama
	gama2.Terminated = "2026-01-06"
	b.PutDaily("2026-01-06", []testkit.Rec{alfa2, gama2})
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	eq(t, "renamed", strs(t, conn, `SELECT name_core FROM search_name WHERE skey = '99900001'`), []string{"alfa domy"})
	if n := num(t, conn, `SELECT count(*) FROM search_word WHERE word = 'stavby'`); n != 0 {
		t.Fatal("a word no subject uses stayed in the dictionary")
	}
	if n := num(t, conn, `SELECT count(*) FROM search_name WHERE skey = '99900003'`); n != 0 {
		t.Fatal("a terminated subject is still searchable")
	}
	if n := num(t, conn, `SELECT count(*) FROM rpo_record WHERE id = $1 AND terminated_on = '2026-01-06'`, gama.ID); n != 1 {
		t.Fatal("termination not stored")
	}

	// --- 5. gap: 01-08 exists, 01-07 does not: wait, never apply out of order ---
	delta := testkit.Company(7, "Delta Nová, s.r.o.")
	b.PutDaily("2026-01-08", []testkit.Rec{delta})
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if st := state(t, conn); st.State != "gap_wait" || st.AppliedThrough != "2026-01-06" {
		t.Fatalf("gap: %+v", st)
	}
	if n := num(t, conn, `SELECT count(*) FROM rpo_record WHERE id = $1`, delta.ID); n != 0 {
		t.Fatal("a later daily file was applied across a gap")
	}

	// --- 6. a new snapshot (01-07) heals the gap: reconcile, absence, then 01-08 ---
	// beta is gone from the snapshot (absent); alfa2 still renamed; gama terminated.
	b.PutSnapshot("2026-01-07", []testkit.Rec{alfa2, gama2, trader, agent, noico}, 3)
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	st = state(t, conn)
	if st.State != "daily" || st.BaseSnapshot != "2026-01-07" || st.AppliedThrough != "2026-01-08" {
		t.Fatalf("after reconcile: %+v", st)
	}
	if n := num(t, conn, `SELECT count(*) FROM rpo_record WHERE id = $1 AND removed_at IS NOT NULL`, beta.ID); n != 1 {
		t.Fatal("a record missing from the snapshot was not marked absent")
	}
	if n := num(t, conn, `SELECT count(*) FROM search_name WHERE skey = '99900002'`); n != 0 {
		t.Fatal("an absent subject is still searchable")
	}
	if n := num(t, conn, `SELECT count(*) FROM search_name WHERE skey = '99900007'`); n != 1 {
		t.Fatal("the daily file after the healed gap was not applied")
	}
	if n := num(t, conn, `SELECT count(*) FROM rpo_seen`); n != 0 {
		t.Fatal("rpo_seen not emptied after the reconcile")
	}

	// --- 7. validation: an oversized daily file blocks; data stays as it was ---
	big := []testkit.Rec{testkit.Company(11, "X1"), testkit.Company(12, "X2"), testkit.Company(13, "X3"), testkit.Company(14, "X4"), testkit.Company(15, "X5")}
	b.PutDaily("2026-01-09", big)
	err := s.Run(ctx)
	if !errors.Is(err, syncer.ErrBlocked) {
		t.Fatalf("want ErrBlocked, got %v", err)
	}
	st = state(t, conn)
	if st.State != "blocked" || st.PreviousState != "daily" || !strings.Contains(st.Diagnostic, "more than 4") {
		t.Fatalf("blocked state: %+v", st)
	}
	if n := num(t, conn, `SELECT count(*) FROM rpo_record WHERE id BETWEEN 900000011 AND 900000015`); n != 0 {
		t.Fatal("a rejected file was partly applied")
	}
	if n := num(t, conn, `SELECT count(*) FROM sync_unit WHERE unit = 'rpo:actual_2026-01-09.json.gz' AND status = 'rejected'`); n != 1 {
		t.Fatal("the rejected unit is not recorded")
	}
	if err := s.Run(ctx); !errors.Is(err, syncer.ErrBlocked) {
		t.Fatalf("a blocked source must stay blocked, got %v", err)
	}

	// --- 8. operator unblocks; the corrected file applies ---
	if err := syncer.Unblock(ctx, conn, "rpo"); err != nil {
		t.Fatal(err)
	}
	b.PutDaily("2026-01-09", big[:2])
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if st := state(t, conn); st.State != "daily" || st.AppliedThrough != "2026-01-09" {
		t.Fatalf("after unblock: %+v", st)
	}

	// --- 9. only one sync at a time ---
	other := testkit.Conn(t, dsn)
	if _, err := other.Exec(ctx, `SELECT pg_advisory_lock($1)`, syncer.LockKey); err != nil {
		t.Fatal(err)
	}
	if err := s.Run(ctx); !errors.Is(err, syncer.ErrLocked) {
		t.Fatalf("want ErrLocked, got %v", err)
	}
}

func TestReconcileAbsenceThresholdBlocks(t *testing.T) {
	ctx := context.Background()
	conn := testkit.Conn(t, testkit.Database(t))
	b := testkit.NewBucket(t)
	bk := rpo.NewBucket(b.URL())
	s := syncer.NewRPO(conn, syncer.RPOConfig{Bucket: bk, WorkDir: t.TempDir(), PartRecords: 10, AbsenceMaxRatio: 0.2, SnapshotMinRatio: 0.1})

	recs := []testkit.Rec{testkit.Company(1, "A"), testkit.Company(2, "B"), testkit.Company(3, "C"), testkit.Company(4, "D")}
	b.PutSnapshot("2026-02-01", recs, 10)
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	// the next snapshot drops half of the records: 50 % > 20 % → blocked, nothing marked
	b.PutSnapshot("2026-03-01", recs[:2], 10)
	if err := s.Run(ctx); !errors.Is(err, syncer.ErrBlocked) {
		t.Fatalf("want ErrBlocked, got %v", err)
	}
	if n := num(t, conn, `SELECT count(*) FROM rpo_record WHERE removed_at IS NOT NULL`); n != 0 {
		t.Fatal("records were marked absent above the threshold")
	}
	if n := num(t, conn, `SELECT count(*) FROM search_name`); n != 4 {
		t.Fatalf("search layer changed: %d subjects", n)
	}
}
