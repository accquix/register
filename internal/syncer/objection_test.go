package syncer_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/accquix/register/internal/ingest"
	"github.com/accquix/register/internal/testkit"
)

func object(t *testing.T, conn *pgx.Conn, key string) int64 {
	t.Helper()
	id, _, err := ingest.AddObjection(context.Background(), conn, key, "ticket "+key)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func searchKeys(t *testing.T, conn *pgx.Conn) []string {
	t.Helper()
	return strs(t, conn, `SELECT DISTINCT skey FROM search_name`)
}

// An objection is bound to the person's records, not to the key they had on the day of the
// objection: it holds when a record gains an IČO and when a record loses one.
func TestObjectionFollowsItsRecords(t *testing.T) {
	ctx := context.Background()
	conn := testkit.Conn(t, testkit.Database(t))
	b := testkit.NewBucket(t)
	s := newSync(t, conn, b)

	alfa := testkit.Company(1, "Alfa Stavby, s.r.o.")
	trader := testkit.Trader(4, "99900004", "Ján Fiktívny - Kaderníctvo")
	agent := testkit.Trader(5, "99900004", "Ján Fiktívny") // second record of the same person
	agent.RegisterCode, agent.RegisterName = "9", "Register finančných agentov"
	noico := testkit.Trader(6, "Neuvedené", "Bez IČO Remeslo")
	b.PutSnapshot("2026-01-03", []testkit.Rec{alfa, trader, agent, noico}, 3)
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	byRecord := object(t, conn, "r900000006")
	byICO := object(t, conn, "99900004")
	eq(t, "after the objections", searchKeys(t, conn), []string{"99900001"})
	eq(t, "anchored to the records that carry the IČO",
		strs(t, conn, `SELECT rpo_id::text FROM objection_record WHERE objection_id = $1`, byICO), []string{"900000004", "900000005"})

	// The record without an IČO gains one; one record of the trader loses its IČO; a new record is
	// created with the suppressed IČO.
	noico2 := noico
	noico2.ICO = "99900006"
	agent2 := agent
	agent2.ICO = "Neuvedené"
	fresh := testkit.Trader(8, "99900004", "Ján Fiktívny - Nová Prevádzka")
	b.PutDaily("2026-01-04", []testkit.Rec{noico2, agent2, fresh})
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	eq(t, "after the keys changed", searchKeys(t, conn), []string{"99900001"})
	// ... and the new record loses the IČO the next day: it was created with it, so it is anchored.
	fresh2 := fresh
	fresh2.ICO = "Neuvedené"
	b.PutDaily("2026-01-05", []testkit.Rec{fresh2})
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	eq(t, "a record created with the IČO stays hidden without it", searchKeys(t, conn), []string{"99900001"})
	if n := num(t, conn, `SELECT count(*) FROM search_word WHERE word IN ('remeslo', 'jan', 'kadernictvo', 'prevadzka')`); n != 0 {
		t.Fatalf("the dictionary keeps %d words that only suppressed subjects use", n)
	}

	// Each objection is revoked on its own, by id; the row stays as the record of the undo.
	if _, err := ingest.RevokeObjection(ctx, conn, byRecord); err != nil {
		t.Fatal(err)
	}
	eq(t, "after one undo", searchKeys(t, conn), []string{"99900001", "99900006"})
	if _, err := ingest.RevokeObjection(ctx, conn, byICO); err != nil {
		t.Fatal(err)
	}
	eq(t, "after both undos", searchKeys(t, conn), []string{"99900001", "99900004", "99900006", "r900000005", "r900000008"})
	if n := num(t, conn, `SELECT count(*) FROM objection WHERE revoked_at IS NOT NULL`); n != 2 {
		t.Fatalf("revoked objections must stay as records, got %d", n)
	}
	if _, err := ingest.RevokeObjection(ctx, conn, byICO); err == nil {
		t.Fatal("revoking twice must fail")
	}
}

// A wrong IČO in one daily file must never hide the subject that really owns it for good, and
// must never put the person's objection at risk (review finding N1).
func TestWrongICODoesNotSpreadAnObjection(t *testing.T) {
	ctx := context.Background()
	conn := testkit.Conn(t, testkit.Database(t))
	b := testkit.NewBucket(t)
	s := newSync(t, conn, b)
	alfa := testkit.Company(1, "Alfa Stavby, s.r.o.")
	trader := testkit.Trader(4, "99900004", "Ján Fiktívny - Kaderníctvo")
	b.PutSnapshot("2026-01-03", []testkit.Rec{alfa, trader}, 3)
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	person := object(t, conn, "99900004")

	// Day 1: the trader's record wrongly carries the company's IČO.
	wrong := trader
	wrong.ICO = "99900001"
	b.PutDaily("2026-01-04", []testkit.Rec{wrong})
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	eq(t, "the company stays searchable, the person stays hidden",
		strs(t, conn, `SELECT skey || ' ' || name_core FROM search_name`), []string{"99900001 alfa stavby"})
	// Day 2: RPO corrects it.
	b.PutDaily("2026-01-05", []testkit.Rec{trader})
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	eq(t, "after the correction", strs(t, conn, `SELECT skey || ' ' || name_core FROM search_name`), []string{"99900001 alfa stavby"})
	if ids, err := ingest.ActiveObjections(ctx, conn, "99900001"); err != nil || len(ids) != 0 {
		t.Fatalf("the company got an objection it never had: %v %v", ids, err)
	}
	if n := num(t, conn, `SELECT count(*) FROM objection WHERE id = $1 AND revoked_at IS NULL`, person); n != 1 {
		t.Fatal("the person's objection is gone")
	}

	// The mirror case: the company's record wrongly carries the person's IČO for a day. It is
	// hidden while it does, and comes back with the correction: it was not anchored.
	alfaWrong := alfa
	alfaWrong.ICO = "99900004"
	b.PutDaily("2026-01-06", []testkit.Rec{alfaWrong})
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	eq(t, "hidden while it carries the suppressed IČO", searchKeys(t, conn), nil)
	b.PutDaily("2026-01-07", []testkit.Rec{alfa})
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	eq(t, "back after the correction", searchKeys(t, conn), []string{"99900001"})
	if n := num(t, conn, `SELECT count(*) FROM objection_record WHERE rpo_id = $1`, alfa.ID); n != 0 {
		t.Fatal("a record that only passed through the IČO was anchored")
	}
}

// Undoing one objection never removes another that was recorded independently for the same
// subject, and the caller is told that the subject is still hidden.
func TestUndoKeepsIndependentObjections(t *testing.T) {
	ctx := context.Background()
	conn := testkit.Conn(t, testkit.Database(t))
	b := testkit.NewBucket(t)
	trader := testkit.Trader(4, "99900004", "Ján Fiktívny - Kaderníctvo")
	agent := testkit.Trader(5, "99900004", "Ján Fiktívny")
	agent.RegisterCode, agent.RegisterName = "9", "Register finančných agentov"
	b.PutSnapshot("2026-01-03", []testkit.Rec{testkit.Company(1, "Alfa Stavby, s.r.o."), trader, agent}, 3)
	if err := newSync(t, conn, b).Run(ctx); err != nil {
		t.Fatal(err)
	}
	for _, order := range [][2]string{{"99900004", "r900000005"}, {"r900000005", "99900004"}, {"99900004", "99900004"}} {
		first, second := object(t, conn, order[0]), object(t, conn, order[1])
		still, err := ingest.RevokeObjection(ctx, conn, first)
		if err != nil {
			t.Fatal(err)
		}
		if len(still) != 1 || still[0] != second {
			t.Fatalf("%v: revoke must report the objection that still hides the subject, got %v", order, still)
		}
		if n := num(t, conn, `SELECT count(*) FROM objection WHERE id = $1 AND revoked_at IS NULL`, second); n != 1 {
			t.Fatalf("%v: the independent objection was removed", order)
		}
		if order[1] == "99900004" {
			eq(t, "one of two undone: still hidden", searchKeys(t, conn), []string{"99900001"})
		} else if n := num(t, conn, `SELECT count(*) FROM search_name WHERE name_core = 'jan fiktivny'`); n != 0 {
			t.Fatalf("%v: the record with its own objection is searchable", order)
		}
		if still, err = ingest.RevokeObjection(ctx, conn, second); err != nil || len(still) != 0 {
			t.Fatalf("%v: second revoke: %v %v", order, still, err)
		}
		eq(t, "both undone", searchKeys(t, conn), []string{"99900001", "99900004"})
	}
}

// An objection recorded while a unit is being applied waits for that unit, so the unit cannot
// commit search rows the objection did not see.
func TestObjectionWaitsForTheUnitBeingApplied(t *testing.T) {
	ctx := context.Background()
	dsn := testkit.Database(t)
	conn := testkit.Conn(t, dsn)
	b := testkit.NewBucket(t)
	b.PutSnapshot("2026-01-03", []testkit.Rec{testkit.Company(1, "Alfa Stavby, s.r.o.")}, 3)
	if err := newSync(t, conn, b).Run(ctx); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, ico, change string }{
		{"a subject already in search", "99900001",
			`UPDATE rpo_record SET name = 'Alfa Nová, s.r.o.', name_core = 'alfa nova', changed_at = now() WHERE ico = '99900001'`},
		{"a subject that first appears in the unit", "99900002",
			`INSERT INTO rpo_record (id, ico, kind, name, name_core, source_as_of, src_date, src_rank, row_hash)
VALUES (900000002, '99900002', 'sole_trader', 'Beta Vymyslená', 'beta vymyslena', '2026-01-04', '2026-01-04', 0, '\x01')`},
	} {
		t.Run(c.name, func(t *testing.T) {
			unit, err := conn.Begin(ctx) // the unit: change the record, rebuild its search rows, stay open
			if err != nil {
				t.Fatal(err)
			}
			defer unit.Rollback(ctx)
			if _, err := unit.Exec(ctx, c.change); err != nil {
				t.Fatal(err)
			}
			if err := ingest.RefreshSubjects(ctx, unit, []string{c.ico}); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			operator := testkit.Conn(t, dsn)
			go func() { _, _, err := ingest.AddObjection(ctx, operator, c.ico, "ticket"); done <- err }()
			select {
			case err := <-done:
				t.Fatalf("the objection did not wait for the open unit (err %v)", err)
			case <-time.After(300 * time.Millisecond):
			}
			if err := unit.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if n := num(t, conn, `SELECT (SELECT count(*) FROM search_name WHERE skey = $1) + (SELECT count(*) FROM search_token WHERE skey = $1)`, c.ico); n != 0 {
				t.Fatalf("%d search rows of a suppressed subject survived", n)
			}
			if n := num(t, conn, `SELECT count(*) FROM search_word WHERE word IN ('alfa', 'nova', 'beta', 'vymyslena')`); n != 0 {
				t.Fatalf("%d dictionary words of suppressed subjects survived", n)
			}
		})
	}
}

// A backup restored into a rebuilt database before its first sync keeps every objection in force,
// including the anchors of records that no longer carry the IČO, and confirms the installation.
func TestRestoreBeforeTheFirstSync(t *testing.T) {
	ctx := context.Background()
	trader := testkit.Trader(4, "99900004", "Ján Fiktívny - Kaderníctvo")
	agent := testkit.Trader(5, "Neuvedené", "Ján Fiktívny") // lost its IČO before the rebuild
	recs := []testkit.Rec{testkit.Company(1, "Alfa Stavby, s.r.o."), trader, agent}

	at := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	backup := []ingest.Objection{{Key: "99900004", Reason: "ticket", CreatedAt: at, Anchors: []int64{900000004, 900000005}}}

	conn := testkit.Conn(t, testkit.Database(t)) // the rebuilt database
	if ok, _ := ingest.SetupConfirmed(ctx, conn); ok {
		t.Fatal("a new database must start unconfirmed")
	}
	added, err := ingest.RestoreObjections(ctx, conn, backup)
	if err != nil || added != 1 {
		t.Fatalf("restore: %d %v", added, err)
	}
	if again, err := ingest.RestoreObjections(ctx, conn, backup); err != nil || again != 0 {
		t.Fatalf("a second restore must add nothing: %d %v", again, err)
	}
	if ok, _ := ingest.SetupConfirmed(ctx, conn); !ok {
		t.Fatal("a restore must confirm the installation")
	}
	if err := ingest.ConfirmNoObjections(ctx, conn); err == nil {
		t.Fatal("--init-empty must refuse on a confirmed installation with objections")
	}
	b := testkit.NewBucket(t)
	b.PutSnapshot("2026-01-03", recs, 3)
	if err := newSync(t, conn, b).Run(ctx); err != nil {
		t.Fatal(err)
	}
	eq(t, "after the rebuild", searchKeys(t, conn), []string{"99900001"})
	list, err := ingest.ListObjections(ctx, conn)
	if err != nil || len(list) != 1 || !list[0].CreatedAt.Equal(at) || len(list[0].Anchors) != 2 {
		t.Fatalf("list: %+v %v", list, err)
	}
}
