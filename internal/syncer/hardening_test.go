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

// OQ-37 (D38, D53): items with no identifier at all are left out and counted, up to the list's cap;
// they change no other threshold, and more of them blocks the list as before. A sudden jump within
// the cap is applied with a warning.
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
	srv.Put("ds_dsrdp", testkit.FSZip("ds_dsrdp", day, append(taxItems(1, 100), noID(11)...)))
	if err := s.Run(ctx); !errors.Is(err, syncer.ErrBlocked) {
		t.Fatalf("over the cap: %v", err)
	}
	if st := state(); st.State != "blocked" || !strings.Contains(st.Diagnostic, "without any identifier") {
		t.Fatalf("over the cap: %+v", st)
	}
	if n := num(t, conn, `SELECT count(*) FROM fs_tax_subject WHERE absent_since IS NULL`); n != 100 {
		t.Fatalf("a blocked version must not change the data: %d", n)
	}

	// D53: a jump within the cap (6, then 41) is applied, with a warning in the diagnostic; the
	// next quiet version clears it. A wider cap stands in for the size of the real list.
	if _, err := conn.Exec(ctx, `UPDATE sync_source SET state = 'daily', diagnostic = NULL WHERE source = 'fs:ds_dsrdp'`); err != nil {
		t.Fatal(err)
	}
	lists := make([]fs.List, len(fs.Lists))
	copy(lists, fs.Lists)
	for i := range lists {
		lists[i].Unidentified = fs.UnidentifiedCap{Max: 1000, Floor: 100}
	}
	wide := syncer.NewFS(conn, syncer.FSConfig{Client: fs.NewClient(srv.URL()), WorkDir: t.TempDir(), SkipMinRows: true, Lists: lists})
	for i, n := range []int{6, 41} {
		srv.Put("ds_dsrdp", testkit.FSZip("ds_dsrdp", day, append(taxItems(1, 100+i), noID(n)...)))
		if err := wide.Run(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if st := state(); st.State != "daily" || st.Cursor.Skipped != 41 || st.Cursor.Items != 101 || !strings.HasPrefix(st.Diagnostic, "warning: 41 items without any identifier, 6") {
		t.Fatalf("a jump within the cap: %+v", st)
	}
	srv.Put("ds_dsrdp", testkit.FSZip("ds_dsrdp", day, append(taxItems(1, 100), noID(41)...)))
	if err := wide.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if st := state(); st.State != "daily" || st.Diagnostic != "" {
		t.Fatalf("a steady count: %+v", st)
	}
}

// A snapshot whose files are replaced in the bucket under the same names must never be published
// as a mix of the two sets (review finding D5). Part 2 of the first set is rejected, the bootstrap
// blocks with part 1 applied; the publisher uploads every part again with other content; after the
// unblock only the new set is published, and each unit records the ETag it was applied from.
func TestReplacedSnapshotIsNeverMixed(t *testing.T) {
	ctx := context.Background()
	conn := testkit.Conn(t, testkit.Database(t))
	b := testkit.NewBucket(t)
	bk := rpo.NewBucket(b.URL())
	bk.Retries = 1
	work := t.TempDir()
	mk := func() *syncer.RPO {
		return syncer.NewRPO(conn, syncer.RPOConfig{Bucket: bk, WorkDir: work, PartRecords: 2})
	}
	co := func(n int) testkit.Rec { return testkit.Company(n, "Firma "+string(rune('A'+n))) }
	manifest := []byte("init_2026-01-03_001.json.gz\ninit_2026-01-03_002.json.gz\ninit_2026-01-03_003.json.gz\n")
	put := func(parts ...[]testkit.Rec) {
		for i, recs := range parts {
			b.Put("batch-init/init_2026-01-03_00"+string(rune('1'+i))+".json.gz", testkit.File("2026-01-03", recs))
		}
		b.Put("batch-init/init_2026-01-03_list.txt", manifest)
	}

	// the first set: part 2 holds one record instead of two, so it is rejected after part 1 applied
	put([]testkit.Rec{co(1), co(2)}, []testkit.Rec{co(3)}, []testkit.Rec{co(5)})
	if err := mk().Run(ctx); !errors.Is(err, syncer.ErrBlocked) {
		t.Fatalf("want a block on part 2, got %v", err)
	}
	if n := num(t, conn, `SELECT count(*) FROM rpo_record`); n != 2 {
		t.Fatalf("records applied from the first set: %d", n)
	}
	// the publisher replaces every part, same names, other records
	put([]testkit.Rec{co(11), co(12)}, []testkit.Rec{co(13), co(14)}, []testkit.Rec{co(15)})
	if err := syncer.Unblock(ctx, conn, "rpo"); err != nil {
		t.Fatal(err)
	}
	if err := mk().Run(ctx); err != nil {
		t.Fatal(err)
	}
	if st := state(t, conn); st.State != "daily" || st.BaseSnapshot != "2026-01-03" {
		t.Fatalf("state: %+v", st)
	}
	eq(t, "only the second set is published", strs(t, conn, `SELECT id::text FROM rpo_record`),
		[]string{"900000011", "900000012", "900000013", "900000014", "900000015"})
	eq(t, "the search layer holds only the second set", strs(t, conn, `SELECT skey FROM search_name`),
		[]string{"99900011", "99900012", "99900013", "99900014", "99900015"})
	if n := num(t, conn, `SELECT count(*) FROM sync_unit WHERE unit LIKE 'rpo:init_%' AND status = 'applied' AND etag IS NOT NULL`); n != 3 {
		t.Fatalf("units with the ETag they were applied from: %d", n)
	}

	// the same during a monthly reconcile: the next snapshot is replaced after its first part
	put2 := func(parts ...[]testkit.Rec) {
		for i, recs := range parts {
			b.Put("batch-init/init_2026-02-07_00"+string(rune('1'+i))+".json.gz", testkit.File("2026-02-07", recs))
		}
		b.Put("batch-init/init_2026-02-07_list.txt", []byte("init_2026-02-07_001.json.gz\ninit_2026-02-07_002.json.gz\ninit_2026-02-07_003.json.gz\n"))
	}
	renamed := co(11)
	renamed.Name = "Firma Stará Verzia"
	put2([]testkit.Rec{renamed, co(12)}, []testkit.Rec{co(13)}, []testkit.Rec{co(14)}) // part 2 short: rejected after part 1
	loose := func() *syncer.RPO {
		return syncer.NewRPO(conn, syncer.RPOConfig{Bucket: bk, WorkDir: work, PartRecords: 2, AbsenceMaxRatio: 0.9, SnapshotMinRatio: 0.1})
	}
	if err := loose().Run(ctx); !errors.Is(err, syncer.ErrBlocked) {
		t.Fatalf("want a block in the reconcile, got %v", err)
	}
	final := co(11)
	final.Name = "Firma Nová Verzia"
	put2([]testkit.Rec{final, co(12)}, []testkit.Rec{co(13), co(14)}, []testkit.Rec{co(16)}) // the corrected set; record 15 is gone
	if err := syncer.Unblock(ctx, conn, "rpo"); err != nil {
		t.Fatal(err)
	}
	if err := loose().Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strs(t, conn, `SELECT name FROM rpo_record WHERE id = 900000011`); got[0] != "Firma Nová Verzia" {
		t.Fatalf("the reconcile kept data of the replaced set: %v", got)
	}
	eq(t, "absent against the final set only", strs(t, conn, `SELECT id::text FROM rpo_record WHERE removed_at IS NOT NULL`), []string{"900000015"})
}

// A file named for a day that has not come is not used. A complete snapshot named 9999-12-31
// used to be picked as the newest, rejected for its export date, and picked again after every
// unblock: only the publisher could clear it (review finding D12).
func TestFilesNamedForTheFutureAreIgnored(t *testing.T) {
	ctx := context.Background()
	conn := testkit.Conn(t, testkit.Database(t))
	b := testkit.NewBucket(t)
	s := newSync(t, conn, b)
	alfa := testkit.Company(1, "Alfa Stavby, s.r.o.")
	b.PutSnapshot("2026-01-03", []testkit.Rec{alfa}, 3)
	b.PutSnapshot("9999-12-31", []testkit.Rec{testkit.Company(9, "Budúcnosť, s.r.o.")}, 3)
	b.PutDaily("9999-12-30", []testkit.Rec{testkit.Company(8, "Zajtrajšok, s.r.o.")})
	for i := 0; i < 2; i++ { // the bootstrap, then a pass on the published database
		if err := s.Run(ctx); err != nil {
			t.Fatal(err)
		}
		if st := state(t, conn); st.State != "daily" || st.BaseSnapshot != "2026-01-03" || st.AppliedThrough != "2026-01-03" {
			t.Fatalf("pass %d: %+v", i, st)
		}
	}
	if n := num(t, conn, `SELECT count(*) FROM rpo_record`); n != 1 {
		t.Fatalf("records: %d", n)
	}
}

// After an upgrade whose migration changes what is hidden, the API does not serve until the sync
// has rebuilt the search layer (review finding D4). The stale search row here stands for one that
// an earlier rule left behind.
func TestSearchIsRebuiltAfterAnUpgrade(t *testing.T) {
	ctx := context.Background()
	conn := testkit.Conn(t, testkit.Database(t))
	b := testkit.NewBucket(t)
	s := newSync(t, conn, b)
	b.PutSnapshot("2026-01-03", []testkit.Rec{testkit.Company(1, "Alfa Stavby, s.r.o."), testkit.Company(2, "Beta Pekáreň, s.r.o.")}, 3)
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	// what a migration leaves: the search rows of one subject are missing, another's are stale,
	// and the rebuild is owed
	if _, err := conn.Exec(ctx, `DELETE FROM search_name WHERE skey = '99900001'`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `UPDATE search_name SET name_core = 'stary nazov' WHERE skey = '99900002'`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `UPDATE register_setup SET search_rebuild_pending = true WHERE only_row`); err != nil {
		t.Fatal(err)
	}
	if err := s.Run(ctx); err != nil { // a pass with nothing new in the bucket
		t.Fatal(err)
	}
	eq(t, "search after the rebuild", strs(t, conn, `SELECT skey || ' ' || name_core FROM search_name`),
		[]string{"99900001 alfa stavby", "99900002 beta pekaren"})
	if n := num(t, conn, `SELECT count(*) FROM register_setup WHERE search_rebuild_pending`); n != 0 {
		t.Fatal("the rebuild is still marked as owed")
	}
	if n := num(t, conn, `SELECT count(*) FROM search_word WHERE word IN ('stary', 'nazov')`); n != 0 {
		t.Fatal("the dictionary kept words of the stale rows")
	}
}

// The rebuild owed after an upgrade is done by any sync pass, also one that syncs only the FS
// lists: a deployment that schedules the sources separately must not stay at 503 for ever
// (independent review, finding 5).
func TestOwedRebuildIsDoneByAnFSOnlyPass(t *testing.T) {
	ctx := context.Background()
	conn := testkit.Conn(t, testkit.Database(t))
	b := testkit.NewBucket(t)
	b.PutSnapshot("2026-01-03", []testkit.Rec{testkit.Company(1, "Alfa Stavby, s.r.o.")}, 3)
	if err := newSync(t, conn, b).Run(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `DELETE FROM search_name WHERE skey = '99900001'`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `UPDATE register_setup SET search_rebuild_pending = true WHERE only_row`); err != nil {
		t.Fatal(err)
	}
	srv := testkit.NewFSServer(t) // no lists published: every list fails to download, the rebuild still runs
	_ = syncer.NewFS(conn, syncer.FSConfig{Client: fs.NewClient(srv.URL()), WorkDir: t.TempDir(), SkipMinRows: true}).Run(ctx)
	eq(t, "search after an FS-only pass", strs(t, conn, `SELECT skey FROM search_name`), []string{"99900001"})
	if n := num(t, conn, `SELECT count(*) FROM register_setup WHERE search_rebuild_pending`); n != 0 {
		t.Fatal("the rebuild is still marked as owed")
	}
	// and by the helper the demo loader uses
	if _, err := conn.Exec(ctx, `UPDATE register_setup SET search_rebuild_pending = true WHERE only_row`); err != nil {
		t.Fatal(err)
	}
	if err := syncer.RebuildIfOwed(ctx, conn, nil); err != nil {
		t.Fatal(err)
	}
	if n := num(t, conn, `SELECT count(*) FROM register_setup WHERE search_rebuild_pending`); n != 0 {
		t.Fatal("RebuildIfOwed left the rebuild owed")
	}
}
