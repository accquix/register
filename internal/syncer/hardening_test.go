package syncer_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/accquix/register/internal/ingest"
	"github.com/accquix/register/internal/sources/fs"
	"github.com/accquix/register/internal/sources/rpo"
	"github.com/accquix/register/internal/syncer"
	"github.com/accquix/register/internal/testkit"
)

// An objection is bound to the subject, not to the key it had on the day of the objection: it
// holds when a record gains an IČO and when a record loses one.
func TestObjectionFollowsKeyChange(t *testing.T) {
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
	for _, key := range []string{"r900000006", "99900004"} {
		if err := ingest.Suppress(ctx, conn, key, "ticket", true); err != nil {
			t.Fatal(err)
		}
	}
	eq(t, "after the objections", strs(t, conn, `SELECT DISTINCT skey FROM search_name`), []string{"99900001"})
	// the IČO objection also covers each record that carries the IČO
	eq(t, "propagated to records", strs(t, conn, `SELECT skey FROM search_suppression WHERE origin = '99900004'`),
		[]string{"r900000004", "r900000005"})

	// The record without an IČO gains one; one record of the trader loses its IČO.
	noico2 := noico
	noico2.ICO = "99900006"
	agent2 := agent
	agent2.ICO = "Neuvedené"
	b.PutDaily("2026-01-04", []testkit.Rec{noico2, agent2})
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	eq(t, "after the keys changed", strs(t, conn, `SELECT DISTINCT skey FROM search_name`), []string{"99900001"})
	eq(t, "propagated to the new IČO", strs(t, conn, `SELECT origin FROM search_suppression WHERE skey = '99900006'`), []string{"r900000006"})
	if n := num(t, conn, `SELECT count(*) FROM search_word WHERE word IN ('remeslo', 'jan', 'kadernictvo')`); n != 0 {
		t.Fatalf("the dictionary keeps %d words that only suppressed subjects use", n)
	}

	// Undo by a propagated key removes the whole objection; the other one stays.
	if err := ingest.Suppress(ctx, conn, "99900006", "", false); err != nil {
		t.Fatal(err)
	}
	eq(t, "after one undo", strs(t, conn, `SELECT DISTINCT skey FROM search_name`), []string{"99900001", "99900006"})
	if n := num(t, conn, `SELECT count(*) FROM search_suppression WHERE skey IN ('99900006', 'r900000006')`); n != 0 {
		t.Fatalf("an undone objection left %d rows", n)
	}
	if err := ingest.Suppress(ctx, conn, "99900004", "", false); err != nil {
		t.Fatal(err)
	}
	eq(t, "after both undos", strs(t, conn, `SELECT DISTINCT skey FROM search_name`),
		[]string{"99900001", "99900004", "99900006", "r900000005"})
	if n := num(t, conn, `SELECT count(*) FROM search_suppression`); n != 0 {
		t.Fatalf("%d objections left", n)
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
			go func() { done <- ingest.Suppress(ctx, operator, c.ico, "ticket", true) }()
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

// A first snapshot is published only when it is whole: the manifest and the bucket agree, and the
// total is large enough to be a register.
func TestBootstrapNeedsTheWholeSnapshot(t *testing.T) {
	ctx := context.Background()
	conn := testkit.Conn(t, testkit.Database(t))
	b := testkit.NewBucket(t)
	var recs []testkit.Rec
	for i := 1; i <= 5; i++ {
		recs = append(recs, testkit.Company(i, "Firma "+string(rune('A'+i))))
	}
	b.PutSnapshot("2026-01-03", recs, 3) // parts 001 (3 records) and 002 (2)
	manifest := "init_2026-01-03_001.json.gz\ninit_2026-01-03_002.json.gz\n"

	// The manifest lists only the first part (a full one), but the bucket already holds a second.
	b.Put("batch-init/init_2026-01-03_list.txt", []byte("init_2026-01-03_001.json.gz\n"))
	if err := newSync(t, conn, b).Run(ctx); err != nil {
		t.Fatal(err)
	}
	if st := state(t, conn); st.State != "empty" || st.PublishedAsOf != "" {
		t.Fatalf("a snapshot whose manifest and parts disagree was used: %+v", st)
	}
	if n := num(t, conn, `SELECT count(*) FROM rpo_record`); n != 0 {
		t.Fatalf("%d records applied from an incomplete snapshot", n)
	}

	// Whole, but far too small for a first publication: blocked, nothing published.
	b.Put("batch-init/init_2026-01-03_list.txt", []byte(manifest))
	bk := rpo.NewBucket(b.URL())
	strict := syncer.NewRPO(conn, syncer.RPOConfig{Bucket: bk, WorkDir: t.TempDir(), PartRecords: 3, BootstrapMinRecords: 10})
	if err := strict.Run(ctx); !errors.Is(err, syncer.ErrBlocked) || !strings.Contains(err.Error(), "fewer than the minimum") {
		t.Fatalf("want a block on the minimum, got %v", err)
	}
	if st := state(t, conn); st.State != "blocked" || st.PublishedAsOf != "" {
		t.Fatalf("after the block: %+v", st)
	}
	if n := num(t, conn, `SELECT count(*) FROM search_name`); n != 0 {
		t.Fatal("the search layer was built for an unpublished snapshot")
	}

	// With the minimum met, the same snapshot publishes.
	if err := syncer.Unblock(ctx, conn, "rpo"); err != nil {
		t.Fatal(err)
	}
	ok := syncer.NewRPO(conn, syncer.RPOConfig{Bucket: bk, WorkDir: t.TempDir(), PartRecords: 3, BootstrapMinRecords: 5})
	if err := ok.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if st := state(t, conn); st.State != "daily" || st.PublishedAsOf != "2026-01-03" {
		t.Fatalf("after the publication: %+v", st)
	}
}

// The export date inside a file feeds the order guard, so a wrong one is rejected before anything
// is staged: a future date would freeze the file's rows against every later correction.
func TestExportDateIsValidated(t *testing.T) {
	ctx := context.Background()
	conn := testkit.Conn(t, testkit.Database(t))
	b := testkit.NewBucket(t)
	s := newSync(t, conn, b)
	alfa := testkit.Company(1, "Alfa Stavby, s.r.o.")
	b.PutSnapshot("2026-01-03", []testkit.Rec{alfa}, 3)
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	alfa2 := alfa
	alfa2.Name = "Alfa Domy, s.r.o."
	const key = "batch-daily/actual_2026-01-04.json.gz"

	for _, c := range []struct{ export, want string }{
		{"2099-01-01", "in the future"},
		{"2025-12-01", "from the date in the file name"},
	} {
		b.Put(key, testkit.File(c.export, []testkit.Rec{alfa2}))
		if err := s.Run(ctx); !errors.Is(err, syncer.ErrBlocked) || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("export date %s: want a block (%s), got %v", c.export, c.want, err)
		}
		if got := strs(t, conn, `SELECT name || ' ' || src_date::text FROM rpo_record WHERE id = $1`, alfa.ID); got[0] != "Alfa Stavby, s.r.o. 2026-01-03" {
			t.Fatalf("a rejected file changed the record: %v", got)
		}
		if err := syncer.Unblock(ctx, conn, "rpo"); err != nil {
			t.Fatal(err)
		}
	}
	b.Put(key, testkit.File("2026-01-04", []testkit.Rec{alfa2}))
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strs(t, conn, `SELECT name FROM rpo_record WHERE id = $1`, alfa.ID); got[0] != "Alfa Domy, s.r.o." {
		t.Fatalf("the corrected file was not applied: %v", got)
	}
}

// A list dated in the future would look fresh for ever and block every correct version after it;
// a large list far below its usual size must not be published as the whole list.
func TestFSRejectsFutureDatesAndTruncatedLists(t *testing.T) {
	ctx := context.Background()
	conn := testkit.Conn(t, testkit.Database(t))
	srv := testkit.NewFSServer(t)
	now := func() time.Time { return time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC) }
	vat := []testkit.Item{testkit.VATItem(1, "99900001", "§4", "01.08.2020", "01.08.2020")}

	srv.Put("ds_dphs", testkit.FSZip("ds_dphs", "2026-11-20", vat))
	srv.Put("ds_dsrdp", testkit.FSZip("ds_dsrdp", "2026-11-11", taxItems(1, 10))) // tomorrow: allowed (time zones)
	lists := []fs.List{{Slug: "ds_dphs", Target: fs.VATPayers, MaxDrop: 0.03}, {Slug: "ds_dsrdp", Target: fs.TaxSubjects, MaxDrop: 0.03}}
	s := syncer.NewFS(conn, syncer.FSConfig{Client: fs.NewClient(srv.URL()), WorkDir: t.TempDir(), Lists: lists, Now: now})
	err := s.Run(ctx)
	if !errors.Is(err, syncer.ErrBlocked) || !strings.Contains(err.Error(), "in the future") {
		t.Fatalf("want a future-date block, got %v", err)
	}
	if n := num(t, conn, `SELECT count(*) FROM fs_vat_payer`); n != 0 {
		t.Fatal("a list dated in the future was applied")
	}
	if st, _ := syncer.LoadState(ctx, conn, "fs:ds_dphs"); st.State != "blocked" || st.PublishedAsOf != "" {
		t.Fatalf("ds_dphs: %+v", st)
	}
	if st, _ := syncer.LoadState(ctx, conn, "fs:ds_dsrdp"); st.State != "daily" || st.PublishedAsOf != "2026-11-11" {
		t.Fatalf("ds_dsrdp (dated tomorrow) must apply: %+v", st)
	}

	// the floor of a large list, on its first load
	floor := []fs.List{{Slug: "ds_iz_ran", Target: fs.ListEntries, MaxDrop: 0.03, MinRows: 50}}
	srv.Put("ds_iz_ran", testkit.FSZip("ds_iz_ran", "2026-11-10", []testkit.Item{{"DIC": "2099900001", "IDS": "spoľahlivý"}}))
	s = syncer.NewFS(conn, syncer.FSConfig{Client: fs.NewClient(srv.URL()), WorkDir: t.TempDir(), Lists: floor, Now: now})
	if err := s.Run(ctx); !errors.Is(err, syncer.ErrBlocked) || !strings.Contains(err.Error(), "fewer than the minimum") {
		t.Fatalf("want a block on the floor, got %v", err)
	}
	if n := num(t, conn, `SELECT count(*) FROM fs_list_entry`); n != 0 {
		t.Fatal("a truncated list was applied")
	}
	// the operator's override
	if err := syncer.Unblock(ctx, conn, "fs:ds_iz_ran"); err != nil {
		t.Fatal(err)
	}
	s = syncer.NewFS(conn, syncer.FSConfig{Client: fs.NewClient(srv.URL()), WorkDir: t.TempDir(), Lists: floor, Now: now, SkipMinRows: true})
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
}

// Undoing one objection never removes another that the operator recorded independently for the
// same subject, whichever of the two came first.
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
	all := []string{"99900004", "r900000004", "r900000005"}
	for _, order := range [][2]string{{"99900004", "r900000005"}, {"r900000005", "99900004"}} {
		for _, key := range order {
			if err := ingest.Suppress(ctx, conn, key, "ticket "+key, true); err != nil {
				t.Fatal(err)
			}
		}
		// undo the first objection: the second still hides the whole subject
		if err := ingest.Suppress(ctx, conn, order[0], "", false); err != nil {
			t.Fatal(err)
		}
		eq(t, "one of two objections undone: still hidden", strs(t, conn, `SELECT DISTINCT skey FROM search_name`), []string{"99900001"})
		eq(t, "the remaining objection covers every key", strs(t, conn, `SELECT skey FROM search_suppression`), all)
		eq(t, "the remaining objection is the operator's own row",
			strs(t, conn, `SELECT skey FROM search_suppression WHERE origin IS NULL`), []string{order[1]})
		// undo the second: only now is the subject back
		if err := ingest.Suppress(ctx, conn, order[1], "", false); err != nil {
			t.Fatal(err)
		}
		eq(t, "both undone", strs(t, conn, `SELECT DISTINCT skey FROM search_name`), []string{"99900001", "99900004"})
		if n := num(t, conn, `SELECT count(*) FROM search_suppression`); n != 0 {
			t.Fatalf("%d objections left", n)
		}
	}
}

// The boundaries of the date rules: one day of skew and "tomorrow" pass, one day more does not.
func TestExportDateBoundaries(t *testing.T) {
	ctx := context.Background()
	const key = "batch-daily/actual_2026-01-04.json.gz"
	for _, c := range []struct {
		name, export string
		now          time.Time
		skew         int
		ok           bool
	}{
		{"equal to the name", "2026-01-04", time.Date(2026, 1, 4, 23, 59, 0, 0, time.UTC), 1, true},
		{"one day after the name, dated tomorrow", "2026-01-05", time.Date(2026, 1, 4, 0, 0, 0, 0, time.UTC), 1, true},
		{"one day before the name", "2026-01-03", time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC), 1, true},
		{"two days after the name", "2026-01-06", time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC), 1, false},
		{"two days before the name", "2026-01-02", time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC), 1, false},
		{"a wider configured skew", "2026-01-06", time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC), 2, true},
		{"within the skew but after tomorrow", "2026-01-06", time.Date(2026, 1, 4, 12, 0, 0, 0, time.UTC), 5, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			conn := testkit.Conn(t, testkit.Database(t))
			b := testkit.NewBucket(t)
			bk := rpo.NewBucket(b.URL())
			bk.Retries = 1
			s := syncer.NewRPO(conn, syncer.RPOConfig{Bucket: bk, WorkDir: t.TempDir(), PartRecords: 3,
				DateSkewDays: c.skew, Now: func() time.Time { return c.now }})
			b.PutSnapshot("2026-01-03", []testkit.Rec{testkit.Company(1, "Alfa Stavby, s.r.o.")}, 3)
			b.Put(key, testkit.File(c.export, []testkit.Rec{testkit.Company(2, "Beta Pekáreň, s.r.o.")}))
			err := s.Run(ctx)
			if c.ok && err != nil {
				t.Fatalf("want the file applied, got %v", err)
			}
			if !c.ok && !errors.Is(err, syncer.ErrBlocked) {
				t.Fatalf("want a block, got %v", err)
			}
			if n := num(t, conn, `SELECT count(*) FROM rpo_record WHERE id = 900000002`); (n == 1) != c.ok {
				t.Fatalf("record applied: %d, want applied: %v", n, c.ok)
			}
		})
	}
}
