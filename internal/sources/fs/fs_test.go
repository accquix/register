package fs_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/accquix/register/internal/sources/fs"
	"github.com/accquix/register/internal/testkit"
)

func write(t *testing.T, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "list.zip")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func readAll(t *testing.T, slug string, data []byte) ([]fs.Row, *fs.Decoder, error) {
	t.Helper()
	d, err := fs.Open(slug, write(t, data))
	if err != nil {
		return nil, nil, err
	}
	t.Cleanup(func() { d.Close() })
	var rows []fs.Row
	for d.Next() {
		rows = append(rows, d.Row())
	}
	return rows, d, d.Err()
}

func TestVATList(t *testing.T) {
	items := []testkit.Item{
		testkit.VATItem(1, "99900001", "§4", "01.08.2020", "01.08.2020"),
		testkit.VATItem(2, "99900002", "§7a", "06.09.2023", ""), // registered, not a payer
		testkit.VATItem(3, "123456789012", "§4b", "01.01.2024", "01.01.2024"),
	}
	items[2]["IC_DPH"] = items[0]["IC_DPH"] // a VAT group member shares the IČ DPH; its IČO is not valid
	rows, d, err := readAll(t, "ds_dphs", testkit.FSZip("ds_dphs", "2026-11-10", items))
	if err != nil {
		t.Fatal(err)
	}
	if d.AsOf().Format("2006-01-02") != "2026-11-10" || len(rows) != 3 {
		t.Fatalf("as of %v, %d rows", d.AsOf(), len(rows))
	}
	a, b, c := rows[0], rows[1], rows[2]
	if a.Key != "SK2099900001|99900001" || a.Paragraph != "§4" || a.D1 != "2020-08-01" || a.D3 != "2020-08-01" || a.PostalCode != "999 99" {
		t.Errorf("payer: %+v", a)
	}
	if b.Paragraph != "§7a" || b.D1 != "2023-09-06" || b.D3 != "" {
		t.Errorf("§7a must have a registration date and no payer date: %+v", b)
	}
	if c.ICO != "" || !strings.HasPrefix(c.Key, "SK2099900001|n:") || c.Key == a.Key {
		t.Errorf("group member without a valid IČO is keyed by name: %+v", c)
	}
}

func TestOtherLists(t *testing.T) {
	rows, _, err := readAll(t, "ds_dpho", testkit.FSZip("ds_dpho", "2026-11-10", []testkit.Item{
		{"IC_DPH": "SK2099900005", "NAZOV": "Osobitná Fiktívna", "ICO": "99900005", "DAT_ZACATIA": "01.10.2020", "ADRESA": "1"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Key != "SK2099900005|01.10.2020|" || rows[0].Attrs != `{"DAT_ZACATIA":"01.10.2020"}` || rows[0].Street != "1" {
		t.Errorf("ds_dpho: %+v", rows[0])
	}
	rows, _, err = readAll(t, "ds_iz_ran", testkit.FSZip("ds_iz_ran", "2026-10-31", []testkit.Item{
		{"DIC": "2099900006", "ICO": "99900006", "IDS": "spoľahlivý", "NAZOV_SUBJEKTU": "Spoľahlivá Fiktívna", "STAT": "SK"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Key != "2099900006" || rows[0].Attrs != `{"IDS":"spoľahlivý"}` || rows[0].Name != "Spoľahlivá Fiktívna" {
		t.Errorf("ds_iz_ran: %+v", rows[0])
	}
}

func TestParseErrorsReject(t *testing.T) {
	cases := map[string]struct {
		slug string
		item testkit.Item
	}{
		"malformed IČO":  {"ds_dsrdp", testkit.Item{"ICO": "123", "NAZOV_DS": "x"}},
		"bad IC_DPH":     {"ds_dphs", testkit.Item{"IC_DPH": "SK12", "DRUH_REG_DPH": "§4"}},
		"bad paragraph":  {"ds_dphs", testkit.Item{"IC_DPH": "SK2099900001", "DRUH_REG_DPH": "§99"}},
		"bad date":       {"ds_dphs", testkit.Item{"IC_DPH": "SK2099900001", "DRUH_REG_DPH": "§4", "DATUM_REG": "2020-08-01"}},
		"missing IC_DPH": {"ds_dphz", testkit.Item{"ICO": "99900001", "DAT_ZVEREJNENIA": "25.02.2021"}},
		"bad DIC":        {"ds_iz_ran", testkit.Item{"DIC": "12"}},
	}
	for name, c := range cases {
		if _, _, err := readAll(t, c.slug, testkit.FSZip(c.slug, "2026-11-10", []testkit.Item{c.item})); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if _, err := fs.Open("ds_dphs", write(t, []byte("not a zip"))); err == nil {
		t.Error("not a zip: want an error")
	}
}

func TestConditionalFetch(t *testing.T) {
	srv := testkit.NewFSServer(t)
	srv.Put("ds_dphs", testkit.FSZip("ds_dphs", "2026-11-10", nil))
	c := fs.NewClient(srv.URL())
	dir := t.TempDir()
	f, err := c.Fetch(context.Background(), "ds_dphs", "", dir)
	if err != nil || f.NotModified || f.ETag == "" || len(f.SHA256) != 64 {
		t.Fatalf("first fetch: %+v %v", f, err)
	}
	g, err := c.Fetch(context.Background(), "ds_dphs", f.ETag, dir)
	if err != nil || !g.NotModified {
		t.Fatalf("second fetch must be 304: %+v %v", g, err)
	}
	if _, err := c.Fetch(context.Background(), "ds_missing", "", dir); err == nil {
		t.Fatal("want an error for a missing list")
	}
}

// unidentified mirrors the real items of 2026-11-11 (OQ-37): a name, sometimes an address, and
// no DIČ, IČO or IČ DPH at all. The names and the address are fictional.
func unidentified(n int) []testkit.Item {
	out := make([]testkit.Item, n)
	for i := range out {
		out[i] = testkit.Item{"NAZOV_DS": fmt.Sprintf("Fiktívna Osoba %d", i)}
		if i%3 == 0 {
			out[i]["OBEC"], out[i]["PSC"], out[i]["ULICA_CISLO"], out[i]["NAZOV_STATU"] = "Fiktívne Mesto", "99999", "Testovacia 1", "Slovensko"
		}
	}
	return out
}

func TestUnidentifiedItemsAreSkipped(t *testing.T) {
	items := []testkit.Item{testkit.TaxItem(1, "99900001")}
	items = append(items, unidentified(3)...)
	items = append(items, testkit.TaxItem(2, "99900002"))
	items = append(items, unidentified(3)...)
	items = append(items, testkit.TaxItem(3, ""))
	rows, d, err := readAll(t, "ds_dsrdp", testkit.FSZip("ds_dsrdp", "2026-11-11", items))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || d.Count() != 3 || d.Skipped() != 6 {
		t.Fatalf("rows %d, count %d, skipped %d", len(rows), d.Count(), d.Skipped())
	}
	if rows[1].DIC != "2099900002" || rows[2].ICO != "" {
		t.Fatalf("the identified items around the skipped ones must be kept as they are: %+v", rows)
	}

	// a small file: the floor of 10 holds; one more rejects it at the end of the file
	ok := append([]testkit.Item{testkit.TaxItem(1, "99900001")}, unidentified(10)...)
	if _, d, err := readAll(t, "ds_dsrdp", testkit.FSZip("ds_dsrdp", "2026-11-11", ok)); err != nil || d.Skipped() != 10 {
		t.Fatalf("at the floor: %v", err)
	}
	over := append(append([]testkit.Item{}, ok...), unidentified(1)...)
	if _, _, err := readAll(t, "ds_dsrdp", testkit.FSZip("ds_dsrdp", "2026-11-11", over)); err == nil || !strings.Contains(err.Error(), "11 of 12 items without any identifier") {
		t.Fatalf("over the floor: %v", err)
	}
	// the hard maximum stops a broken file early, before its end
	if _, _, err := readAll(t, "ds_dsrdp", testkit.FSZip("ds_dsrdp", "2026-11-11", unidentified(1001))); err == nil || !strings.Contains(err.Error(), "more than 1000") {
		t.Fatalf("over the maximum: %v", err)
	}
	// an operator's override for one run (REGISTER_FS_MAX_UNIDENTIFIED)
	d, err = fs.Open("ds_dsrdp", write(t, testkit.FSZip("ds_dsrdp", "2026-11-11", over)))
	if err != nil {
		t.Fatal(err)
	}
	d.SetUnidentified(fs.UnidentifiedCap{Max: 11, Floor: 11})
	for d.Next() {
	}
	if d.Close(); d.Err() != nil || d.Skipped() != 11 {
		t.Fatalf("override: %v", d.Err())
	}

	// a new sole trader: a valid IČO, no DIČ yet. Not an error, and nothing for the DIČ list to hold
	newTrader := []testkit.Item{testkit.TaxItem(1, "99900001"), {"ICO": "99900002", "NAZOV_DS": "Nová Živnosť"}}
	if rows, d, err := readAll(t, "ds_dsrdp", testkit.FSZip("ds_dsrdp", "2026-11-11", newTrader)); err != nil || len(rows) != 1 || d.Skipped() != 0 || d.NoDICYet() != 1 {
		t.Fatalf("IČO without DIČ: %v %d", err, len(rows))
	}

	// D52: new businesses are normal and have no limit; they do not count against the cap of items
	// without any identifier, which still holds next to them
	many := []testkit.Item{testkit.TaxItem(1, "99900001")}
	for i := range 300 {
		many = append(many, testkit.Item{"ICO": fmt.Sprintf("9991%04d", i), "NAZOV_DS": "Nová Živnosť"})
	}
	many = append(many, unidentified(10)...)
	if rows, d, err := readAll(t, "ds_dsrdp", testkit.FSZip("ds_dsrdp", "2026-11-11", many)); err != nil || len(rows) != 1 ||
		d.NoDICYet() != 300 || d.Skipped() != 10 {
		t.Fatalf("300 new businesses and 10 unidentified items: %v", err)
	}
	if _, _, err := readAll(t, "ds_dsrdp", testkit.FSZip("ds_dsrdp", "2026-11-11", append(many, unidentified(1)...))); err == nil {
		t.Fatal("the cap on items without any identifier must still hold next to new businesses")
	}

	// a malformed identifier is not "no identifier": it still rejects, with the item's position
	bad := []testkit.Item{testkit.TaxItem(1, "99900001"), {"NAZOV_DS": "x"}, {"DIC": "12", "NAZOV_DS": "y"}}
	if _, _, err := readAll(t, "ds_dsrdp", testkit.FSZip("ds_dsrdp", "2026-11-11", bad)); err == nil || !strings.Contains(err.Error(), "item 3:") {
		t.Fatalf("a malformed DIČ after a skipped item: %v", err)
	}
}

// D53: the cap follows the list's size. On the DIČ list of 2026-11-12 (1 423 331 items) the 41 real
// items pass with a wide margin, 800 would block it; the other lists stay between 10 and 100.
func TestUnidentifiedCap(t *testing.T) {
	dsrdp, _ := fs.ListBySlug("ds_dsrdp")
	dphs, _ := fs.ListBySlug("ds_dphs")
	cases := []struct {
		name  string
		c     fs.UnidentifiedCap
		items int
		want  int
	}{
		{"DIČ list, real size", dsrdp.Unidentified, 1_423_331, 712},
		{"DIČ list, small file", dsrdp.Unidentified, 100, 10},
		{"DIČ list, huge file", dsrdp.Unidentified, 5_000_000, 1000},
		{"VAT payers", dphs.Unidentified, 400_000, 40},
		{"VAT payers, huge", dphs.Unidentified, 5_000_000, 100},
		{"default for a small list", fs.UnidentifiedCap{}, 3000, 10},
	}
	for _, c := range cases {
		if got := c.c.Allowed(c.items); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
	if a := dsrdp.Unidentified.Allowed(1_423_331); 41 > a || 800 <= a {
		t.Fatalf("41 must pass and 800 block: cap %d", a)
	}
}
