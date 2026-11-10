package syncer_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/accquix/register/internal/sources/fs"
	"github.com/accquix/register/internal/syncer"
	"github.com/accquix/register/internal/testkit"
)

func taxItems(from, to int) []testkit.Item {
	var out []testkit.Item
	for i := from; i <= to; i++ {
		out = append(out, testkit.TaxItem(i, ""))
	}
	return out
}

func TestFSLifecycle(t *testing.T) {
	ctx := context.Background()
	dsn := testkit.Database(t)
	conn := testkit.Conn(t, dsn)
	srv := testkit.NewFSServer(t)
	s := syncer.NewFS(conn, syncer.FSConfig{Client: fs.NewClient(srv.URL()), WorkDir: t.TempDir()})

	// --- 1. first load of all six lists ---
	vat := []testkit.Item{
		testkit.VATItem(1, "99900001", "§4", "01.08.2020", "01.08.2020"),
		testkit.VATItem(2, "99900002", "§7a", "06.09.2023", ""),
	}
	srv.Put("ds_dphs", testkit.FSZip("ds_dphs", "2026-11-10", vat))
	srv.Put("ds_dsrdp", testkit.FSZip("ds_dsrdp", "2026-11-09", taxItems(1, 100)))
	srv.Put("ds_iz_ran", testkit.FSZip("ds_iz_ran", "2026-10-31", []testkit.Item{{"DIC": "2099900001", "ICO": "99900001", "IDS": "spoľahlivý"}}))
	srv.Put("ds_dphz", testkit.FSZip("ds_dphz", "2026-11-10", []testkit.Item{{"IC_DPH": "SK2099900003", "ROK_PORUSENIA": "2020", "DAT_ZVEREJNENIA": "25.02.2021"}}))
	srv.Put("ds_dphv", testkit.FSZip("ds_dphv", "2026-11-10", nil))
	dup := testkit.Item{"IC_DPH": "SK2099900004", "DAT_ZACATIA": "01.10.2020", "NAZOV": "A"}
	dup2 := testkit.Item{"IC_DPH": "SK2099900004", "DAT_ZACATIA": "01.10.2020", "NAZOV": "B"} // same key, other content
	srv.Put("ds_dpho", testkit.FSZip("ds_dpho", "2026-11-10", []testkit.Item{dup, dup, dup2}))
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	for _, l := range fs.Lists {
		st, err := syncer.LoadState(ctx, conn, syncer.Source(l.Slug))
		if err != nil || st.State != "daily" || st.PublishedAsOf == "" || st.Cursor.ETag == "" {
			t.Fatalf("%s: %+v %v", l.Slug, st, err)
		}
	}
	if n := num(t, conn, `SELECT count(*) FROM fs_tax_subject`); n != 100 {
		t.Fatalf("tax subjects: %d", n)
	}
	if got := strs(t, conn, `SELECT paragraph || ':' || coalesce(payer_from::text, '-') FROM fs_vat_payer`); strings.Join(got, ",") != "§4:2020-08-01,§7a:-" {
		t.Fatalf("vat: %v", got)
	}
	if got := strs(t, conn, `SELECT name FROM fs_list_entry WHERE dataset = 'ds_dpho'`); strings.Join(got, ",") != "A" {
		t.Fatalf("duplicate keys must keep one deterministic row: %v", got)
	}
	if got := strs(t, conn, `SELECT detail FROM sync_unit WHERE source = 'fs:ds_dpho'`); !strings.Contains(got[0], "2 duplicates (1 conflicting)") {
		t.Fatalf("duplicates not reported: %v", got)
	}

	// --- 2. nothing new: every list answers 304 ---
	gets := srv.Gets["ds_dsrdp"]
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if srv.Gets["ds_dsrdp"] != gets {
		t.Fatal("an unchanged list was downloaded again")
	}

	// --- 3. a new version: 2 removed, 1 changed, 1 added (2 % absent: within 3 %) ---
	items := taxItems(3, 101)
	items[0]["OBEC"] = "Iné Mesto"
	srv.Put("ds_dsrdp", testkit.FSZip("ds_dsrdp", "2026-11-10", items))
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if n := num(t, conn, `SELECT count(*) FROM fs_tax_subject WHERE absent_since IS NOT NULL`); n != 2 {
		t.Fatalf("absent: %d", n)
	}
	if n := num(t, conn, `SELECT count(*) FROM fs_tax_subject WHERE dic = '2099900003' AND municipality = 'Iné Mesto'`); n != 1 {
		t.Fatal("changed row not updated")
	}
	if st, _ := syncer.LoadState(ctx, conn, "fs:ds_dsrdp"); st.PublishedAsOf != "2026-11-10" {
		t.Fatalf("list date not published: %+v", st)
	}

	// --- 4. a list that shrank by 10 % blocks only itself; the data stays ---
	srv.Put("ds_dsrdp", testkit.FSZip("ds_dsrdp", "2026-11-11", taxItems(13, 101)))
	srv.Put("ds_dphs", testkit.FSZip("ds_dphs", "2026-11-11", append(vat, testkit.VATItem(9, "99900009", "§4", "01.11.2026", "01.11.2026"))))
	err := s.Run(ctx)
	if !errors.Is(err, syncer.ErrBlocked) || !strings.Contains(err.Error(), "ds_dsrdp") {
		t.Fatalf("want ds_dsrdp blocked, got %v", err)
	}
	if n := num(t, conn, `SELECT count(*) FROM fs_tax_subject WHERE absent_since IS NULL`); n != 99 {
		t.Fatalf("a rejected list changed the data: %d active", n)
	}
	if n := num(t, conn, `SELECT count(*) FROM fs_vat_payer`); n != 3 {
		t.Fatal("another list was held back by the blocked one")
	}

	// --- 5. the operator accepts the smaller list: unblock and run with a higher threshold ---
	if err := syncer.Unblock(ctx, conn, "fs:ds_dsrdp"); err != nil {
		t.Fatal(err)
	}
	loose := syncer.NewFS(conn, syncer.FSConfig{Client: fs.NewClient(srv.URL()), WorkDir: t.TempDir(), MaxDrop: 0.2})
	if err := loose.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if n := num(t, conn, `SELECT count(*) FROM fs_tax_subject WHERE absent_since IS NULL`); n != 89 {
		t.Fatalf("after accepting: %d active", n)
	}

	// --- 6. an older list date is rejected ---
	srv.Put("ds_dphz", testkit.FSZip("ds_dphz", "2026-11-01", nil))
	if err := s.Run(ctx); !errors.Is(err, syncer.ErrBlocked) || !strings.Contains(err.Error(), "older than") {
		t.Fatalf("want an older-date block, got %v", err)
	}
}
