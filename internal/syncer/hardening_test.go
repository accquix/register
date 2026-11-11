package syncer_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/accquix/register/internal/sources/fs"
	"github.com/accquix/register/internal/sources/rpo"
	"github.com/accquix/register/internal/syncer"
	"github.com/accquix/register/internal/testkit"
)

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

// A file name with an impossible date matches the pattern but is not a snapshot: it is ignored,
// and it must never make a pass spin (review: 18 748 manifest requests in 4 s) or hide a valid
// older snapshot from a bootstrap.
func TestImpossibleDatesInFileNamesAreIgnored(t *testing.T) {
	ctx := context.Background()
	junk := func(b *testkit.Bucket) {
		b.Put("batch-init/init_2026-02-31_001.json.gz", testkit.File("2026-02-31", []testkit.Rec{testkit.Company(9, "Nemožná, s.r.o.")}))
		b.Put("batch-init/init_2026-02-31_list.txt", []byte("init_2026-02-31_001.json.gz\n"))
		b.Put("batch-daily/actual_2026-13-01.json.gz", testkit.File("2026-13-01", nil))
	}
	run := func(s *syncer.RPO) error {
		done := make(chan error, 1)
		go func() { done <- s.Run(ctx) }()
		select {
		case err := <-done:
			return err
		case <-time.After(20 * time.Second):
			t.Fatal("the pass did not end")
			return nil
		}
	}

	// an empty database: the valid older snapshot is used
	conn := testkit.Conn(t, testkit.Database(t))
	b := testkit.NewBucket(t)
	b.PutSnapshot("2026-01-03", []testkit.Rec{testkit.Company(1, "Alfa Stavby, s.r.o.")}, 3)
	junk(b)
	s := newSync(t, conn, b)
	if err := run(s); err != nil {
		t.Fatal(err)
	}
	if st := state(t, conn); st.State != "daily" || st.BaseSnapshot != "2026-01-03" {
		t.Fatalf("bootstrap with a malformed snapshot in the bucket: %+v", st)
	}
	// a published database: the pass ends at once and nothing changes
	if err := run(s); err != nil {
		t.Fatal(err)
	}
	if n := b.Gets["batch-init/init_2026-02-31_list.txt"] + b.Gets["batch-init/init_2026-02-31_001.json.gz"]; n != 0 {
		t.Fatalf("a malformed snapshot was requested %d times", n)
	}
	if n := num(t, conn, `SELECT count(*) FROM rpo_record`); n != 1 {
		t.Fatalf("records: %d", n)
	}
}

// Order is decided by the date in the file name. A snapshot part whose exportDate is a day later
// than its name must not outrank the daily file of that later day (it used to: the daily change
// was dropped while applied_through advanced).
func TestSnapshotDoesNotOutrankTheNextDaily(t *testing.T) {
	ctx := context.Background()
	conn := testkit.Conn(t, testkit.Database(t))
	b := testkit.NewBucket(t)
	s := newSync(t, conn, b)
	alfa := testkit.Company(1, "Alfa Stavby, s.r.o.")
	b.PutSnapshot("2026-01-03", []testkit.Rec{alfa}, 3)
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	// the next snapshot (01-07) is stamped 01-08 inside; the daily file of 01-08 renames the subject
	b.Put("batch-init/init_2026-01-07_001.json.gz", testkit.File("2026-01-08", []testkit.Rec{alfa}))
	b.Put("batch-init/init_2026-01-07_list.txt", []byte("init_2026-01-07_001.json.gz\n"))
	renamed := alfa
	renamed.Name = "Alfa Domy, s.r.o."
	b.PutDaily("2026-01-08", []testkit.Rec{renamed})
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if st := state(t, conn); st.BaseSnapshot != "2026-01-07" || st.AppliedThrough != "2026-01-08" {
		t.Fatalf("state: %+v", st)
	}
	if got := strs(t, conn, `SELECT name || ' ' || src_date::text || ' ' || source_as_of::text FROM rpo_record WHERE id = $1`, alfa.ID); got[0] != "Alfa Domy, s.r.o. 2026-01-08 2026-01-08" {
		t.Fatalf("the daily change after the snapshot was lost: %v", got)
	}
	eq(t, "search", strs(t, conn, `SELECT name_core FROM search_name`), []string{"alfa domy"})
}

// A snapshot is used only when it has settled, when it is not clearly smaller than the previous
// one in the bucket, and a bootstrap that moves to another snapshot starts over.
func TestBootstrapTrustsOnlyASettledWholeSnapshot(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 1, 20, 12, 0, 0, 0, time.UTC)
	mk := func(conn *pgx.Conn, b *testkit.Bucket) *syncer.RPO {
		bk := rpo.NewBucket(b.URL())
		bk.Retries = 1
		return syncer.NewRPO(conn, syncer.RPOConfig{Bucket: bk, WorkDir: t.TempDir(), PartRecords: 2, Now: func() time.Time { return now }})
	}
	var recs []testkit.Rec
	for i := 1; i <= 6; i++ {
		recs = append(recs, testkit.Company(i, "Firma "+string(rune('A'+i))))
	}

	t.Run("still being published", func(t *testing.T) {
		conn := testkit.Conn(t, testkit.Database(t))
		b := testkit.NewBucket(t)
		b.PutSnapshot("2026-01-10", recs, 2)
		b.Modified["batch-init/init_2026-01-10_003.json.gz"] = now.Add(-time.Hour) // the last part arrived an hour ago
		if err := mk(conn, b).Run(ctx); err != nil {
			t.Fatal(err)
		}
		if st := state(t, conn); st.State != "empty" {
			t.Fatalf("an unsettled snapshot was used: %+v", st)
		}
		b.Modified["batch-init/init_2026-01-10_003.json.gz"] = now.Add(-3 * time.Hour)
		if err := mk(conn, b).Run(ctx); err != nil {
			t.Fatal(err)
		}
		if st := state(t, conn); st.State != "daily" {
			t.Fatalf("a settled snapshot was not used: %+v", st)
		}
	})

	t.Run("clearly smaller than the previous snapshot", func(t *testing.T) {
		conn := testkit.Conn(t, testkit.Database(t))
		b := testkit.NewBucket(t)
		b.PutSnapshot("2026-01-03", recs, 2)     // 3 parts
		b.PutSnapshot("2026-01-10", recs[:2], 2) // 1 part: two parts fewer
		if err := mk(conn, b).Run(ctx); !errors.Is(err, syncer.ErrBlocked) || !strings.Contains(err.Error(), "previous snapshot") {
			t.Fatalf("want a block, got %v", err)
		}
		if st := state(t, conn); st.PublishedAsOf != "" {
			t.Fatalf("published: %+v", st)
		}
	})

	t.Run("moving to a newer snapshot starts over", func(t *testing.T) {
		conn := testkit.Conn(t, testkit.Database(t))
		b := testkit.NewBucket(t)
		// snapshot A: its first part applies, its second (last) part is oversized and is rejected
		b.Put("batch-init/init_2026-01-03_001.json.gz", testkit.File("2026-01-03", recs[:2]))
		b.Put("batch-init/init_2026-01-03_002.json.gz", testkit.File("2026-01-03", recs[2:5]))
		b.Put("batch-init/init_2026-01-03_list.txt", []byte("init_2026-01-03_001.json.gz\ninit_2026-01-03_002.json.gz\n"))
		if err := mk(conn, b).Run(ctx); !errors.Is(err, syncer.ErrBlocked) {
			t.Fatalf("want a block on the oversized part, got %v", err)
		}
		if n := num(t, conn, `SELECT count(*) FROM rpo_record`); n != 2 {
			t.Fatalf("records of the abandoned snapshot: %d", n)
		}
		// snapshot B arrives with other records
		b.PutSnapshot("2026-01-10", recs[4:], 2)
		if err := syncer.Unblock(ctx, conn, "rpo"); err != nil {
			t.Fatal(err)
		}
		if err := mk(conn, b).Run(ctx); err != nil {
			t.Fatal(err)
		}
		if st := state(t, conn); st.State != "daily" || st.BaseSnapshot != "2026-01-10" {
			t.Fatalf("state: %+v", st)
		}
		eq(t, "only the published snapshot's records", strs(t, conn, `SELECT id::text FROM rpo_record`), []string{"900000005", "900000006"})
	})
}

// OQ-37 (D38): items with no identifier at all are left out and counted, up to fs.MaxUnidentified
// per file; they change no other threshold, and more of them blocks the list as before.
func TestFSSkipsItemsWithoutAnyIdentifier(t *testing.T) {
	ctx := context.Background()
	conn := testkit.Conn(t, testkit.Database(t))
	srv := testkit.NewFSServer(t)
	s := syncer.NewFS(conn, syncer.FSConfig{Client: fs.NewClient(srv.URL()), WorkDir: t.TempDir(), SkipMinRows: true})
	noID := func(n int) []testkit.Item {
		out := make([]testkit.Item, n)
		for i := range out {
			out[i] = testkit.Item{"NAZOV_DS": "Fiktívna Osoba", "OBEC": "Fiktívne Mesto"}
		}
		return out
	}
	day := time.Now().UTC().Format("2006-01-02")
	for _, l := range fs.Lists {
		if l.Slug != "ds_dsrdp" {
			srv.Put(l.Slug, testkit.FSZip(l.Slug, day, nil))
		}
	}
	state := func() syncer.State {
		st, err := syncer.LoadState(ctx, conn, syncer.Source("ds_dsrdp"))
		if err != nil {
			t.Fatal(err)
		}
		return st
	}

	srv.Put("ds_dsrdp", testkit.FSZip("ds_dsrdp", day, append(taxItems(1, 100), noID(6)...)))
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if st := state(); st.State != "daily" || st.Cursor.Skipped != 6 {
		t.Fatalf("six unidentified items must not block: %+v", st)
	}
	if n := num(t, conn, `SELECT count(*) FROM fs_tax_subject WHERE absent_since IS NULL`); n != 100 {
		t.Fatalf("tax subjects: %d", n)
	}

	// the next version without them: the count goes back to 0, nothing is marked absent
	srv.Put("ds_dsrdp", testkit.FSZip("ds_dsrdp", day, taxItems(1, 100)))
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if st := state(); st.State != "daily" || st.Cursor.Skipped != 0 {
		t.Fatalf("second version: %+v", st)
	}
	if n := num(t, conn, `SELECT count(*) FROM fs_tax_subject WHERE absent_since IS NULL`); n != 100 {
		t.Fatalf("tax subjects after the second version: %d", n)
	}

	// over the cap: the list is blocked and the published data stays
	srv.Put("ds_dsrdp", testkit.FSZip("ds_dsrdp", day, append(taxItems(1, 100), noID(fs.MaxUnidentified+1)...)))
	if err := s.Run(ctx); !errors.Is(err, syncer.ErrBlocked) {
		t.Fatalf("over the cap: %v", err)
	}
	if st := state(); st.State != "blocked" || !strings.Contains(st.Diagnostic, "without any identifier") {
		t.Fatalf("over the cap: %+v", st)
	}
	if n := num(t, conn, `SELECT count(*) FROM fs_tax_subject WHERE absent_since IS NULL`); n != 100 {
		t.Fatalf("a blocked version must not change the data: %d", n)
	}
}
