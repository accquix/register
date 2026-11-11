package syncer_test

import (
	"bytes"
	"context"
	"io"
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

// links lists "record basis source released?" of an objection.
func links(t *testing.T, conn *pgx.Conn, id int64) []string {
	t.Helper()
	return strs(t, conn, `SELECT rpo_id || ' ' || basis || ' ' || coalesce(source, '-') || CASE WHEN released_at IS NULL THEN '' ELSE ' released' END
FROM objection_record WHERE objection_id = $1`, id)
}

// An objection is bound to the person's records, not to the key they had on the day of the
// objection: it holds when a record gains an IČO and when a record loses one, and every link says
// where it came from.
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
	later := testkit.Trader(7, "Neuvedené", "Ján Fiktívny - Neskôr") // gains the objected IČO later
	b.PutSnapshot("2026-01-03", []testkit.Rec{alfa, trader, agent, noico, later}, 3)
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	byRecord := object(t, conn, "r900000006")
	byICO := object(t, conn, "99900004")
	eq(t, "after the objections", searchKeys(t, conn), []string{"99900001", "r900000007"})
	eq(t, "linked by the operator's command", links(t, conn, byICO),
		[]string{"900000004 at_objection -", "900000005 at_objection -"})

	// The record without an IČO gains one; one record of the trader loses its IČO; a new record
	// is created with the objected IČO; an existing record gains the objected IČO.
	noico2 := noico
	noico2.ICO = "99900006"
	agent2 := agent
	agent2.ICO = "Neuvedené"
	fresh := testkit.Trader(8, "99900004", "Ján Fiktívny - Nová Prevádzka")
	later2 := later
	later2.ICO = "99900004"
	b.PutDaily("2026-01-04", []testkit.Rec{noico2, agent2, fresh, later2})
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	eq(t, "after the keys changed", searchKeys(t, conn), []string{"99900001"})
	eq(t, "the sync linked the new carriers, and says from which file", links(t, conn, byICO), []string{
		"900000004 at_objection -", "900000005 at_objection -",
		"900000007 carried_ico rpo:actual_2026-01-04.json.gz", "900000008 carried_ico rpo:actual_2026-01-04.json.gz"})

	// The next day both lose the IČO again: they were linked while they carried it, so they stay
	// hidden (review findings D2/D3: no record leaves an objection without the operator).
	fresh2 := fresh
	fresh2.ICO = "Neuvedené"
	b.PutDaily("2026-01-05", []testkit.Rec{fresh2, later})
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	eq(t, "records that carried the IČO stay hidden without it", searchKeys(t, conn), []string{"99900001"})
	if n := num(t, conn, `SELECT count(*) FROM search_word WHERE word IN ('remeslo', 'jan', 'kadernictvo', 'prevadzka', 'neskor')`); n != 0 {
		t.Fatalf("the dictionary keeps %d words that only suppressed subjects use", n)
	}

	// Each objection is revoked on its own, by id; the row stays as the record of the undo.
	if c, err := ingest.RevokeObjection(ctx, conn, byRecord); err != nil || c.Hidden != 0 {
		t.Fatalf("revoke: %+v %v", c, err)
	}
	eq(t, "after one undo", searchKeys(t, conn), []string{"99900001", "99900006"})
	if c, err := ingest.RevokeObjection(ctx, conn, byICO); err != nil || c.Hidden != 0 || c.Records != 4 {
		t.Fatalf("revoke: %+v %v", c, err)
	}
	eq(t, "after both undos", searchKeys(t, conn),
		[]string{"99900001", "99900004", "99900006", "r900000005", "r900000007", "r900000008"})
	if n := num(t, conn, `SELECT count(*) FROM objection WHERE revoked_at IS NOT NULL`); n != 2 {
		t.Fatalf("revoked objections must stay as records, got %d", n)
	}
	if _, err := ingest.RevokeObjection(ctx, conn, byICO); err == nil {
		t.Fatal("revoking twice must fail")
	}
}

// A wrong IČO in a source file. The two requirements (the person stays hidden; another subject is
// not hidden for good) cannot both be decided by a rule, so the register errs towards hiding,
// never spreads the objection to other records, says what it did, and lets the operator release
// exactly one link, for good (review findings N1 and D1).
func TestWrongICOIsContainedAndCorrectable(t *testing.T) {
	ctx := context.Background()
	conn := testkit.Conn(t, testkit.Database(t))
	b := testkit.NewBucket(t)
	s := newSync(t, conn, b)
	alfa := testkit.Company(1, "Alfa Stavby, s.r.o.")
	alfaBranch := testkit.Company(2, "Alfa Stavby - Pobočka, s.r.o.") // a second record of the company
	alfaBranch.ICO = alfa.ICO
	trader := testkit.Trader(4, "99900004", "Ján Fiktívny - Kaderníctvo")
	b.PutSnapshot("2026-01-03", []testkit.Rec{alfa, alfaBranch, trader}, 3)
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	person := object(t, conn, "99900004")
	names := func() []string { return strs(t, conn, `SELECT skey || ' ' || name_core FROM search_name`) }
	company := []string{"99900001 alfa stavby", "99900001 alfa stavby pobocka"}

	// N1: the person's record wrongly carries the company's IČO for a day. The objection does not
	// spread to the company: its own records stay searchable, the person's record stays hidden.
	wrong := trader
	wrong.ICO = alfa.ICO
	b.PutDaily("2026-01-04", []testkit.Rec{wrong})
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	eq(t, "the company is not hidden by the person's objection", names(), company)
	b.PutDaily("2026-01-05", []testkit.Rec{trader})
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	eq(t, "after the correction", names(), company)
	if ids, err := ingest.ActiveObjections(ctx, conn, alfa.ICO); err != nil || len(ids) != 0 {
		t.Fatalf("the company got an objection it never had: %v %v", ids, err)
	}

	// D1: one record of the company wrongly carries the person's IČO for a day. It is linked (the
	// register cannot know it is a mistake), with its source, and stays hidden after the
	// correction: a documented limitation. Only that record: the company's other record stays.
	alfaWrong := alfa
	alfaWrong.ICO = "99900004"
	b.PutDaily("2026-01-06", []testkit.Rec{alfaWrong})
	b.PutDaily("2026-01-07", []testkit.Rec{alfa})
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	eq(t, "only the record that carried the IČO is hidden", names(), company[1:])
	eq(t, "the link names its source", links(t, conn, person),
		[]string{"900000001 carried_ico rpo:actual_2026-01-06.json.gz", "900000004 at_objection -"})

	// The operator releases that one link. The person's objection and its other link are untouched.
	hidden, err := ingest.ReleaseLink(ctx, conn, person, alfa.ID, "ticket 7: not this person", true)
	if err != nil || hidden {
		t.Fatalf("release: hidden=%v %v", hidden, err)
	}
	eq(t, "after the release", names(), company)
	if n := num(t, conn, `SELECT count(*) FROM objection WHERE id = $1 AND revoked_at IS NULL`, person); n != 1 {
		t.Fatal("releasing a link revoked the objection")
	}
	// The exclusion holds: the same mistake in a later file does not hide the record again, not
	// even while it carries the objected IČO.
	b.PutDaily("2026-01-08", []testkit.Rec{alfaWrong})
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	eq(t, "a released record is not linked again while it carries the IČO", names(), []string{"99900001 alfa stavby pobocka", "99900004 alfa stavby"})
	b.PutDaily("2026-01-09", []testkit.Rec{alfa})
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	eq(t, "and not after the correction", names(), company)
	eq(t, "the release is on record", links(t, conn, person),
		[]string{"900000001 carried_ico rpo:actual_2026-01-06.json.gz released", "900000004 at_objection -"})

	// The operator can withdraw the release; the record named by an objection cannot be released.
	if hidden, err := ingest.ReleaseLink(ctx, conn, person, alfa.ID, "", false); err != nil || !hidden {
		t.Fatalf("unrelease: hidden=%v %v", hidden, err)
	}
	eq(t, "after the release is withdrawn", names(), company[1:])
	direct := object(t, conn, "r900000002")
	if _, err := ingest.ReleaseLink(ctx, conn, direct, alfaBranch.ID, "", true); err == nil {
		t.Fatal("the record an objection names directly must not be releasable")
	}
	if _, err := ingest.ReleaseLink(ctx, conn, person, trader.ID, "", false); err == nil {
		t.Fatal("withdrawing a release that does not exist must fail")
	}
}

// An objection recorded before the first import covers the record when it appears, also when
// daily files inside the bootstrap change the record or take its IČO away (review finding D3).
func TestObjectionBeforeTheFirstImport(t *testing.T) {
	ctx := context.Background()
	conn := testkit.Conn(t, testkit.Database(t))
	id := object(t, conn, "99900004")
	b := testkit.NewBucket(t)
	trader := testkit.Trader(4, "99900004", "Ján Fiktívny - Kaderníctvo")
	agent := testkit.Trader(5, "99900004", "Ján Fiktívny")
	b.PutSnapshot("2026-01-03", []testkit.Rec{testkit.Company(1, "Alfa Stavby, s.r.o."), trader, agent}, 3)
	moved := trader // changed inside the bootstrap
	moved.Municipality = "Iná Fiktívna Obec"
	lost := agent // loses the IČO inside the bootstrap
	lost.ICO = "Neuvedené"
	b.PutDaily("2026-01-04", []testkit.Rec{moved, lost})
	if err := newSync(t, conn, b).Run(ctx); err != nil {
		t.Fatal(err)
	}
	if st := state(t, conn); st.State != "daily" || st.AppliedThrough != "2026-01-04" {
		t.Fatalf("state: %+v", st)
	}
	eq(t, "after the first publication", searchKeys(t, conn), []string{"99900001"})
	eq(t, "linked inside the bootstrap, part by part", links(t, conn, id), []string{
		"900000004 carried_ico rpo:init_2026-01-03_001.json.gz", "900000005 carried_ico rpo:init_2026-01-03_001.json.gz"})
}

// Undoing one objection never removes another that was recorded independently for the same
// subject, and the caller is told how much is still hidden.
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
	for _, c := range []struct {
		first, second string
		stillHidden   int // records of the first objection still hidden after it is revoked
	}{
		{"99900004", "r900000005", 1}, // the second covers one of the two records
		{"r900000005", "99900004", 1},
		{"99900004", "99900004", 2},
	} {
		first, second := object(t, conn, c.first), object(t, conn, c.second)
		cov, err := ingest.RevokeObjection(ctx, conn, first)
		if err != nil {
			t.Fatal(err)
		}
		if cov.Hidden != c.stillHidden {
			t.Fatalf("%s then %s: revoke must report %d record(s) still hidden, got %+v", c.first, c.second, c.stillHidden, cov)
		}
		if n := num(t, conn, `SELECT count(*) FROM objection WHERE id = $1 AND revoked_at IS NULL`, second); n != 1 {
			t.Fatalf("%s then %s: the independent objection was removed", c.first, c.second)
		}
		if n := num(t, conn, `SELECT count(*) FROM search_name WHERE name_core = 'jan fiktivny'`); n != 0 {
			t.Fatalf("%s then %s: a record with its own objection is searchable", c.first, c.second)
		}
		if cov, err = ingest.RevokeObjection(ctx, conn, second); err != nil || cov.Hidden != 0 {
			t.Fatalf("second revoke: %+v %v", cov, err)
		}
		eq(t, "both undone", searchKeys(t, conn), []string{"99900001", "99900004"})
	}
}

// The import and an objection never disagree, wherever the objection lands inside the unit. The
// unit creates a new record with IČO 99900004 and is stopped at an exact stage; the operator
// records the objection there; then the unit finishes. Whatever the order, the record is not
// searchable, it is linked, and it stays hidden when it loses the IČO (review finding D2).
func TestObjectionAtEveryStageOfAUnit(t *testing.T) {
	ctx := context.Background()
	trader := testkit.Trader(8, "99900004", "Ján Fiktívny - Nová Prevádzka")
	file := func(date string, recs ...testkit.Rec) func() (io.ReadCloser, error) {
		data := testkit.File(date, recs)
		return func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
	}
	for _, c := range []struct {
		stage string
		waits bool // the objection must wait for the unit: the unit already holds the objection lock
	}{
		{"staged", false},   // COPY done, nothing merged: the record does not exist yet
		{"merged", false},   // rpo_record written, uncommitted; the unit has not read the objections yet
		{"refreshed", true}, // search rows written, about to commit
	} {
		for _, refresh := range []bool{true, false} { // a daily unit, and a bootstrap unit (no search refresh)
			name := c.stage + " daily"
			if !refresh {
				name = c.stage + " bootstrap"
			}
			t.Run(name, func(t *testing.T) {
				dsn := testkit.Database(t)
				conn, operator, check := testkit.Conn(t, dsn), testkit.Conn(t, dsn), testkit.Conn(t, dsn)
				reached, resume, unitDone, objected := make(chan struct{}), make(chan struct{}), make(chan error, 1), make(chan error, 1)
				go func() {
					_, err := ingest.ApplyRPO(ctx, conn, ingest.RPOUnit{
						Name: "rpo:actual_2026-01-04.json.gz", SHA256: "x", Rank: 0, RefreshSearch: refresh,
						Open: file("2026-01-04", testkit.Company(1, "Alfa Stavby, s.r.o."), trader),
						OnStage: func(stage string) {
							if stage == c.stage {
								close(reached)
								<-resume
							}
						},
					})
					unitDone <- err
				}()
				<-reached
				go func() { _, _, err := ingest.AddObjection(ctx, operator, "99900004", "ticket"); objected <- err }()
				waited := false
				select {
				case err := <-objected:
					if err != nil {
						t.Fatal(err)
					}
					objected <- nil
				case <-time.After(400 * time.Millisecond):
					waited = true
				}
				if waited != c.waits {
					t.Fatalf("objection waited for the unit: %v, want %v", waited, c.waits)
				}
				close(resume)
				if err := <-unitDone; err != nil {
					t.Fatal(err)
				}
				if err := <-objected; err != nil {
					t.Fatal(err)
				}
				if !refresh { // a bootstrap publishes later: build the search layer as it would
					tx, err := check.Begin(ctx)
					if err != nil {
						t.Fatal(err)
					}
					if err := ingest.RebuildSearch(ctx, tx); err != nil {
						t.Fatal(err)
					}
					if err := tx.Commit(ctx); err != nil {
						t.Fatal(err)
					}
				}
				eq(t, "search after the unit and the objection", searchKeys(t, check), []string{"99900001"})
				if n := num(t, check, `SELECT count(*) FROM objection_record WHERE rpo_id = $1 AND released_at IS NULL`, trader.ID); n != 1 {
					t.Fatalf("the record is not linked to the objection (%d links)", n)
				}
				if n := num(t, check, `SELECT count(*) FROM search_token WHERE skey IN ('99900004', 'r900000008')`) +
					num(t, check, `SELECT count(*) FROM search_word WHERE word IN ('jan', 'fiktivny', 'prevadzka')`); n != 0 {
					t.Fatalf("%d tokens or dictionary words of the suppressed record survived", n)
				}
				// the next day the record loses its IČO: it must stay out of search
				lost := trader
				lost.ICO = "Neuvedené"
				if _, err := ingest.ApplyRPO(ctx, check, ingest.RPOUnit{Name: "rpo:actual_2026-01-05.json.gz", SHA256: "y", Rank: 0,
					RefreshSearch: true, Open: file("2026-01-05", lost)}); err != nil {
					t.Fatal(err)
				}
				eq(t, "search after the record lost its IČO", searchKeys(t, check), []string{"99900001"})
			})
		}
	}
}

// The reverse order: the objection's transaction is open (it holds the lock) when a unit that
// creates a matching record reaches the point where it reads the objections. The unit waits, then
// sees the objection.
func TestUnitWaitsForAnObjectionBeingRecorded(t *testing.T) {
	ctx := context.Background()
	dsn := testkit.Database(t)
	conn, check := testkit.Conn(t, dsn), testkit.Conn(t, dsn)
	// hold the objection lock the way AddObjection does, with the objection inserted but uncommitted
	operator := testkit.Conn(t, dsn)
	tx, err := operator.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(0x4163_5265_674f_626a)); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO objection (ico) VALUES ('99900004')`); err != nil {
		t.Fatal(err)
	}
	data := testkit.File("2026-01-04", []testkit.Rec{testkit.Company(1, "Alfa Stavby, s.r.o."), testkit.Trader(8, "99900004", "Ján Fiktívny")})
	done := make(chan error, 1)
	go func() {
		_, err := ingest.ApplyRPO(ctx, conn, ingest.RPOUnit{Name: "rpo:actual_2026-01-04.json.gz", SHA256: "x", Rank: 0, RefreshSearch: true,
			Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the unit did not wait for the open objection (err %v)", err)
	case <-time.After(400 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	eq(t, "search", searchKeys(t, check), []string{"99900001"})
	if n := num(t, check, `SELECT count(*) FROM objection_record WHERE rpo_id = 900000008 AND basis = 'carried_ico'`); n != 1 {
		t.Fatal("the unit did not link the record to the objection it waited for")
	}
}

// A backup restored into a rebuilt database keeps every objection in force, with its links and
// its releases, and confirms the installation.
func TestRestoreKeepsLinksAndReleases(t *testing.T) {
	ctx := context.Background()
	trader := testkit.Trader(4, "99900004", "Ján Fiktívny - Kaderníctvo")
	agent := testkit.Trader(5, "Neuvedené", "Ján Fiktívny") // lost its IČO before the rebuild
	other := testkit.Trader(9, "99900004", "Iná Osoba")     // released before the rebuild: not this person
	recs := []testkit.Rec{testkit.Company(1, "Alfa Stavby, s.r.o."), trader, agent, other}

	at := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	released := at.Add(time.Hour)
	backup := []ingest.Objection{{Key: "99900004", Reason: "ticket", CreatedAt: at, Links: []ingest.Link{
		{RecordID: 900000004, Basis: ingest.BasisAtObjection, CreatedAt: at},
		{RecordID: 900000005, Basis: ingest.BasisCarriedICO, Source: "rpo:actual_2025-12-01.json.gz", CreatedAt: at},
		{RecordID: 900000009, Basis: ingest.BasisCarriedICO, CreatedAt: at, ReleasedAt: &released, ReleasedReason: "ticket 7"},
	}}}

	for _, order := range []string{"restore before the first sync", "restore after the first sync"} {
		t.Run(order, func(t *testing.T) {
			conn := testkit.Conn(t, testkit.Database(t)) // the rebuilt database
			b := testkit.NewBucket(t)
			b.PutSnapshot("2026-01-03", recs, 3)
			if order == "restore after the first sync" {
				if err := newSync(t, conn, b).Run(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if ok, _ := ingest.SetupConfirmed(ctx, conn); ok {
				t.Fatal("a new database must start unconfirmed")
			}
			if added, err := ingest.RestoreObjections(ctx, conn, backup); err != nil || added != 1 {
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
			if err := newSync(t, conn, b).Run(ctx); err != nil {
				t.Fatal(err)
			}
			eq(t, "the person is hidden, the released record is not", searchKeys(t, conn), []string{"99900001", "99900004"})
			eq(t, "only the released record is under the IČO", strs(t, conn, `SELECT name_core FROM search_name WHERE skey = '99900004'`), []string{"ina osoba"})
			list, err := ingest.ListObjections(ctx, conn)
			if err != nil || len(list) != 1 || !list[0].CreatedAt.Equal(at) || len(list[0].Links) != 3 ||
				list[0].Links[1].Source != "rpo:actual_2025-12-01.json.gz" || list[0].Links[2].ReleasedAt == nil || list[0].Links[2].ReleasedReason != "ticket 7" {
				t.Fatalf("list: %+v %v", list, err)
			}
		})
	}
}

// A restore never brings a revoked objection back to life, and a backup carries revocations: a
// revoked entry is restored as revoked (review of the "a backup can only grow" rule).
func TestRestoreNeverReactivatesARevokedObjection(t *testing.T) {
	ctx := context.Background()
	trader := testkit.Trader(4, "99900004", "Ján Fiktívny - Kaderníctvo")
	other := testkit.Trader(6, "99900006", "Iná Fiktívna")
	recs := []testkit.Rec{testkit.Company(1, "Alfa Stavby, s.r.o."), trader, other}
	at := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	revoked := at.Add(24 * time.Hour)

	// 1. the database revoked an objection after the backup was taken: restoring the older backup
	//    (where it is still active) leaves it revoked
	conn := testkit.Conn(t, testkit.Database(t))
	b := testkit.NewBucket(t)
	b.PutSnapshot("2026-01-03", recs, 3)
	if err := newSync(t, conn, b).Run(ctx); err != nil {
		t.Fatal(err)
	}
	id := object(t, conn, "99900004")
	list, err := ingest.ListObjections(ctx, conn) // the backup, taken while the objection is active
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ingest.RevokeObjection(ctx, conn, id); err != nil {
		t.Fatal(err)
	}
	if added, err := ingest.RestoreObjections(ctx, conn, list); err != nil || added != 0 {
		t.Fatalf("restore over a revoked objection: %d %v", added, err)
	}
	if n := num(t, conn, `SELECT count(*) FROM objection WHERE id = $1 AND revoked_at IS NOT NULL`, id); n != 1 {
		t.Fatal("a restore re-activated a revoked objection")
	}
	eq(t, "the subject stays in search", searchKeys(t, conn), []string{"99900001", "99900004", "99900006"})

	// 2. a rebuilt database: the backup holds one active and one revoked objection; the revoked
	//    one is restored as revoked and hides nothing
	fresh := testkit.Conn(t, testkit.Database(t))
	backup := []ingest.Objection{
		{Key: "99900004", CreatedAt: at, RevokedAt: &revoked, Links: []ingest.Link{{RecordID: 900000004, Basis: ingest.BasisAtObjection, CreatedAt: at}}},
		{Key: "99900006", CreatedAt: at},
	}
	if added, err := ingest.RestoreObjections(ctx, fresh, backup); err != nil || added != 2 {
		t.Fatalf("restore: %d %v", added, err)
	}
	b2 := testkit.NewBucket(t)
	b2.PutSnapshot("2026-01-03", recs, 3)
	if err := newSync(t, fresh, b2).Run(ctx); err != nil {
		t.Fatal(err)
	}
	eq(t, "only the active objection hides", searchKeys(t, fresh), []string{"99900001", "99900004"})
	if n := num(t, fresh, `SELECT count(*) FROM objection WHERE ico = '99900004' AND revoked_at = $1`, revoked); n != 1 {
		t.Fatal("the revocation was not restored")
	}
	if n := num(t, fresh, `SELECT count(*) FROM objection_record a JOIN objection o ON o.id = a.objection_id WHERE o.revoked_at IS NOT NULL AND a.basis <> 'at_objection'`); n != 0 {
		t.Fatalf("the sync added %d links to a revoked objection", n)
	}
}
